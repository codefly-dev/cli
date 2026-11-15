package imagedigest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

// ErrNotPublished is a registry's answer that it holds no manifest under a
// reference.
var ErrNotPublished = errors.New("the registry holds no such image")

// registryTimeout bounds one registry read.
const registryTimeout = 30 * time.Second

// Registry is the digest of the manifest image's tag names at its registry
// now: what any machine pulling the tag would get.
func Registry(ctx context.Context, image string) (string, error) {
	repository, ref, err := Repository(image)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, registryTimeout)
	defer cancel()
	descriptor, err := repository.Resolve(ctx, ref)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return "", fmt.Errorf("%w: %s", ErrNotPublished, image)
		}
		return "", fmt.Errorf("resolve %s at its registry: %w", image, err)
	}
	return descriptor.Digest.String(), nil
}

// registryHTTP is the process's one registry client, authenticated the way
// docker is: the docker configuration and its credential helpers.
var registryHTTP = sync.OnceValues(func() (*auth.Client, error) {
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return nil, fmt.Errorf("load the docker registry credentials: %w", err)
	}
	return &auth.Client{
		Cache:      auth.NewCache(),
		Credential: oncePerHost(credentials.Credential(store)),
	}, nil
})

// oncePerHost asks the credential store once per registry host for a few
// minutes. A credential helper is a process (docker-credential-gcloud runs
// gcloud), and docker itself pays for it on every command; one render asks the
// same host dozens of times. The answer is short-lived because a helper's
// credential may be (an access token is), and a run can outlive it. A failure
// is not kept, so the next read asks again.
func oncePerHost(read auth.CredentialFunc) auth.CredentialFunc {
	type answer struct {
		credential auth.Credential
		at         time.Time
	}
	var mu sync.Mutex
	known := map[string]answer{}
	return func(ctx context.Context, host string) (auth.Credential, error) {
		mu.Lock()
		defer mu.Unlock()
		if kept, ok := known[host]; ok && time.Since(kept.at) < registryCredentialLifetime {
			return kept.credential, nil
		}
		credential, err := read(ctx, host)
		if err == nil {
			known[host] = answer{credential, time.Now()}
		}
		return credential, err
	}
}

// registryCredentialLifetime is how long a credential helper's answer is
// reused: well inside the hour an access token it hands out lives.
const registryCredentialLifetime = 5 * time.Minute

// Repository resolves an image reference the way docker does (a bare
// `golang:1.25` is docker.io/library/golang:1.25) to its repository and the
// tag or digest to fetch.
func Repository(image string) (*remote.Repository, string, error) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return nil, "", fmt.Errorf("parse image reference %s: %w", image, err)
	}
	named = reference.TagNameOnly(named)
	var ref string
	switch typed := named.(type) {
	case reference.Digested:
		ref = typed.Digest().String()
	case reference.Tagged:
		ref = typed.Tag()
	}
	repository, err := remote.NewRepository(named.Name())
	if err != nil {
		return nil, "", err
	}
	client, err := registryHTTP()
	if err != nil {
		return nil, "", err
	}
	repository.Client = client
	repository.PlainHTTP = loopbackRegistry(reference.Domain(named))
	return repository, ref, nil
}

// loopbackRegistry is a registry on this machine, which docker and buildx reach
// over plain HTTP.
func loopbackRegistry(host string) bool {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
