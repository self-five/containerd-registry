package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/defaults"
	"github.com/containerd/containerd/reference/docker"

	"github.com/rogpeppe/ociregistry"
	"github.com/rogpeppe/ociregistry/ociserver"
)

// caller responsible for client.Close!
func newContainerdClient() (*containerd.Client, error) {
	// TODO environment variables (CONTAINERD_ADDRESS, CONTAINERD_NAMESPACE)
	return containerd.New(
		defaults.DefaultAddress,
		containerd.WithDefaultNamespace("default"),
	)
}

type containerdRegistry struct {
	*ociregistry.Funcs
	client *containerd.Client
}

func (r containerdRegistry) Repositories(ctx context.Context) ociregistry.Iter[string] {
	is := r.client.ImageService()

	images, err := is.List(ctx)
	if err != nil {
		return ociregistry.ErrorIter[string](err)
	}

	names := []string{}
	for _, image := range images {
		// image.Name is a fully qualified name like "repo:tag" or "repo@digest" so we need to parse it so we can return just the repo name list
		ref, err := docker.ParseNormalizedNamed(image.Name)
		if err != nil {
			// just ignore images whose names we can't parse (TODO debug log?)
			continue
		}
		repo := ref.Name()
		if len(names) > 0 && names[len(names)-1] == repo {
			// "List" returns sorted order, so we only need to check the last item in the list to dedupe
			continue
		}
		names = append(names, repo)
	}

	return ociregistry.SliceIter[string](names)
}

func (r containerdRegistry) Tags(ctx context.Context, repo string) ociregistry.Iter[string] {

	is := r.client.ImageService()

	images, err := is.List(ctx, "name~="+strconv.Quote("^"+regexp.QuoteMeta(repo)+":"))
	if err != nil {
		return ociregistry.ErrorIter[string](err)
	}

	tags := []string{}
	for _, image := range images {
		// image.Name is a fully qualified name like "repo:tag" or "repo@digest" so we need to parse it so we can return just the tags
		ref, err := docker.Parse(image.Name)
		if err != nil {
			// just ignore images whose names we can't parse (TODO debug log?)
			continue
		}
		// TODO do we trust the filter we provided to List(), or do we verify that ref is a ref.Named _and_ that Name() == repo?
		if _, ok := ref.(docker.Digested); ok {
			// ignore "digested" references (foo:bar@baz)
			continue
		}
		if tagged, ok := ref.(docker.Tagged); ok {
			tags = append(tags, tagged.Tag())
		}
	}

	return ociregistry.SliceIter[string](tags)
}

type containerdBlobReader struct {
	client *containerd.Client
	ctx    context.Context
	desc   ociregistry.Descriptor

	readerAt content.ReaderAt
	reader   io.Reader
}

func (br *containerdBlobReader) validate() error {
	info, err := br.client.ContentStore().Info(br.ctx, br.desc.Digest)
	if err != nil {
		return err
	}
	// add Size/MediaType to our descriptor for poor ociregistry's sake (Content-Length/Content-Type headers)
	if br.desc.Size == 0 && info.Size != 0 {
		br.desc.Size = info.Size
	}
	if br.desc.MediaType == "" {
		br.desc.MediaType = "application/octet-stream"
	}
	return nil
}

func (br *containerdBlobReader) ensureReaderAt() (content.ReaderAt, error) {
	if br.readerAt == nil {
		var err error
		br.readerAt, err = br.client.ContentStore().ReaderAt(br.ctx, br.desc)
		if err != nil {
			return nil, err
		}
	}
	return br.readerAt, nil
}

func (br *containerdBlobReader) ensureReader() (io.Reader, error) {
	if br.reader == nil {
		ra, err := br.ensureReaderAt()
		if err != nil {
			return nil, err
		}
		br.reader = content.NewReader(ra)
	}
	return br.reader, nil
}

func (br *containerdBlobReader) Read(p []byte) (int, error) {
	r, err := br.ensureReader()
	if err != nil {
		return 0, err
	}
	return r.Read(p)
}

func (br *containerdBlobReader) Descriptor() ociregistry.Descriptor {
	return br.desc
}

func (br *containerdBlobReader) Close() error {
	return br.readerAt.Close()
}

// containerd.Client becomes owned by the returned containerdBlobReader (or Close'd by this method on error)
func newContainerdBlobReaderFromDescriptor(ctx context.Context, client *containerd.Client, desc ociregistry.Descriptor) (*containerdBlobReader, error) {
	br := &containerdBlobReader{
		client: client,
		ctx:    ctx,
		desc:   desc,
	}

	// let's verify the blob exists (and add size to the descriptor, if it's missing)
	if err := br.validate(); err != nil {
		br.Close()
		return nil, err
	}

	return br, nil
}

// containerd.Client becomes owned by the returned containerdBlobReader (or Close'd by this method on error)
func newContainerdBlobReaderFromDigest(ctx context.Context, client *containerd.Client, digest ociregistry.Digest) (*containerdBlobReader, error) {
	return newContainerdBlobReaderFromDescriptor(ctx, client, ociregistry.Descriptor{
		// this is technically not a valid Descriptor, but containerd's content store is addressed by digest so it works fine ("[containerd/content.Provider.]ReaderAt only requires desc.Digest to be set.")
		Digest: digest,
	})
}

func (r containerdRegistry) GetBlob(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.BlobReader, error) {
	// TODO convert not found into proper 404 errors
	return newContainerdBlobReaderFromDigest(ctx, r.client, digest)
}

func (r containerdRegistry) GetManifest(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.BlobReader, error) {

	// we can technically just return the manifest directly from the content store, but we need the "right" MediaType value for the Content-Type header (and thanks to https://github.com/opencontainers/image-spec/security/advisories/GHSA-77vh-xpmg-72qh we can safely assume manifests have "mediaType" set for us to parse this value out of or else they're manifests we don't care to support!)
	desc := ociregistry.Descriptor{Digest: digest}

	ra, err := r.client.ContentStore().ReaderAt(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer ra.Close()
	mediaTypeWrapper := struct {
		MediaType string `json:"mediaType"`
	}{}
	// TODO add a limitedreader here to make sure we don't read an enormous amount of valid but useless JSON that DoS's us
	if err := json.NewDecoder(content.NewReader(ra)).Decode(&mediaTypeWrapper); err != nil {
		return nil, err
	}
	if mediaTypeWrapper.MediaType == "" {
		return nil, errors.New("failed to parse mediaType") // TODO better error
	}
	desc.Size = ra.Size()
	desc.MediaType = mediaTypeWrapper.MediaType

	return newContainerdBlobReaderFromDescriptor(ctx, r.client, desc)
	// TODO convert not found into proper 404 errors
}

func (r containerdRegistry) GetTag(ctx context.Context, repo string, tagName string) (ociregistry.BlobReader, error) {
	is := r.client.ImageService()

	img, err := is.Get(ctx, repo+":"+tagName)
	if err != nil {
		return nil, err
	}

	return &containerdBlobReader{
		client: r.client,
		ctx:    ctx,
		desc:   img.Target,
	}, nil
}

func (r containerdRegistry) ResolveBlob(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.Descriptor, error) {
	blobReader, err := r.GetBlob(ctx, repo, digest)
	if err != nil {
		return ociregistry.Descriptor{}, err
	}
	defer blobReader.Close()

	return blobReader.Descriptor(), nil
}

func (r containerdRegistry) ResolveManifest(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.Descriptor, error) {
	blobReader, err := r.GetManifest(ctx, repo, digest)
	if err != nil {
		return ociregistry.Descriptor{}, err
	}
	defer blobReader.Close()

	return blobReader.Descriptor(), nil
}

func (r containerdRegistry) ResolveTag(ctx context.Context, repo string, tagName string) (ociregistry.Descriptor, error) {
	blobReader, err := r.GetTag(ctx, repo, tagName)
	if err != nil {
		return ociregistry.Descriptor{}, err
	}
	defer blobReader.Close()

	return blobReader.Descriptor(), nil
}

func main() {
	client, err := newContainerdClient()
	if err != nil {
		log.Fatal(err)
	}
	server := ociserver.New(&containerdRegistry{
		client: client,
	}, nil)
	println("listening on http://*:5000")
	// TODO listen address/port should be configurable somehow
	panic(http.ListenAndServe(":5000", server))
}
