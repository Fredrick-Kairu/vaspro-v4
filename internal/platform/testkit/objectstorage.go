//go:build integration

package testkit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/objectstore"
)

// seaweedfsImage is the same S3-compatible store compose runs
// (deploy/compose/docker-compose.yml), pinned in both places.
const seaweedfsImage = "chrislusf/seaweedfs:4.48"

// Local-only S3 credentials, the same ones compose configures
// (deploy/compose/seaweedfs-s3.json). Not secrets: they only open a
// throwaway test container.
const (
	objectStorageAccessKey = "vaspro-local"
	objectStorageSecretKey = "vaspro-local-secret" //nolint:gosec // G101: a throwaway test container's credential
)

// seaweedfsS3Config gives the S3 gateway one identity. Without one,
// SeaweedFS only allows anonymous requests and refuses signed uploads.
const seaweedfsS3Config = `{"identities":[{"name":"vaspro","credentials":[{"accessKey":"` +
	objectStorageAccessKey + `","secretKey":"` + objectStorageSecretKey +
	`"}],"actions":["Admin","Read","Write","List","Tagging"]}]}`

// StartObjectStorage starts SeaweedFS's S3 gateway and returns how to
// reach it (Bucket left empty for the caller) once a real S3 call
// succeeds. Cleans itself up via t.Cleanup.
func StartObjectStorage(t *testing.T) objectstore.Config {
	t.Helper()
	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx, seaweedfsImage,
		testcontainers.WithCmd("server", "-dir=/data", "-s3", "-s3.config=/etc/seaweedfs/s3.json", "-ip.bind=0.0.0.0"),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader: strings.NewReader(seaweedfsS3Config), ContainerFilePath: "/etc/seaweedfs/s3.json", FileMode: 0o644,
		}),
		testcontainers.WithExposedPorts("8333/tcp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("8333/tcp").WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("testkit: failed to start object storage: %v", err)
	}
	testcontainers.CleanupContainer(t, ctr)
	endpoint, err := ctr.PortEndpoint(ctx, "8333/tcp", "")
	if err != nil {
		t.Fatalf("testkit: object storage endpoint: %v", err)
	}
	cfg := objectstore.Config{Endpoint: endpoint, AccessKey: objectStorageAccessKey, SecretKey: objectStorageSecretKey}

	// SeaweedFS answers on the port before its filer is ready; prove a
	// bucket can be created the way callers will.
	probe := cfg
	probe.Bucket = "testkit-ready"
	store, err := objectstore.New(probe)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitUntilReady(ctx, "object storage", 60*time.Second, 500*time.Millisecond, store.EnsureBucket); err != nil {
		t.Fatalf("testkit: %v", err)
	}
	return cfg
}
