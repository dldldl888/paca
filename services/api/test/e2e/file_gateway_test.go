package e2e_test

import (
	"context"
	"github.com/Paca-AI/api/internal/platform/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBusinessStorageGatewayCutover(t *testing.T) {
	if sharedMinIOEndpoint == "" {
		t.Fatal("real MinIO required")
	}
	dir := os.Getenv("PACA_CADDY_CONFIG_DIR")
	if dir == "" {
		dir = "../../../../deploy/caddy"
	}
	ctx := context.Background()
	for _, template := range []string{"Caddyfile", "Caddyfile.dev"} {
		t.Run(template, func(t *testing.T) {
			config, err := os.ReadFile(filepath.Join(dir, template))
			if err != nil {
				t.Fatal(err)
			}
			// Substitute only the upstream address; all matchers/order remain from the template.
			config = []byte(strings.ReplaceAll(string(config), "minio:9000", sharedMinIOEndpoint))
			path := filepath.Join(t.TempDir(), "Caddyfile")
			if err = os.WriteFile(path, config, 0600); err != nil {
				t.Fatal(err)
			}
			c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: testcontainers.ContainerRequest{Image: "caddy:2-alpine", ExposedPorts: []string{"80/tcp"}, Files: []testcontainers.ContainerFile{{HostFilePath: path, ContainerFilePath: "/etc/caddy/Caddyfile", FileMode: 0600}}, WaitingFor: wait.ForListeningPort("80/tcp").WithStartupTimeout(20 * time.Second)}, Started: true})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Terminate(ctx)
			host, _ := c.Host(ctx)
			port, _ := c.MappedPort(ctx, "80/tcp")
			base := "http://" + host + ":" + port.Port() + "/storage"
			store, err := storage.NewS3Client(ctx, storage.S3Config{Endpoint: "http://" + sharedMinIOEndpoint, Region: "us-east-1", AccessKeyID: "minioadmin", SecretAccessKey: "minioadmin", ForcePathStyle: true, PublicURL: base})
			if err != nil {
				t.Fatal(err)
			}
			bucket := "gateway-" + uuid.NewString()
			if err = store.EnsureBucket(ctx, bucket); err != nil {
				t.Fatal(err)
			}
			perform := func(method, u, body string, want int) string {
				t.Helper()
				req, e := http.NewRequestWithContext(ctx, method, u, strings.NewReader(body))
				if e != nil {
					t.Fatal("build signed request failed")
				}
				req.Header.Set("Content-Type", "text/plain")
				resp, e := http.DefaultClient.Do(req)
				if e != nil {
					t.Fatalf("signed request failed (%T)", e)
				}
				defer resp.Body.Close()
				b, e := io.ReadAll(resp.Body)
				if e != nil {
					t.Fatal("read signed response failed")
				}
				t.Logf("%s %s status=%d bytes=%d", method, req.URL.Path, resp.StatusCode, len(b))
				if resp.StatusCode != want {
					t.Errorf("gateway status=%d want=%d", resp.StatusCode, want)
				}
				return resp.Header.Get("ETag")
			}
			for _, key := range []string{"tasks/" + uuid.NewString() + "/file", "docs/" + uuid.NewString() + "/file", "annotations/" + uuid.NewString() + "/file", "avatars/users/" + uuid.NewString() + "/" + uuid.NewString() + "/upload"} {
				put, e := store.PresignPutObject(ctx, bucket, key, "text/plain", time.Minute)
				if e != nil {
					t.Fatal("presign put failed")
				}
				perform("PUT", put, "secret", 200)
				get, e := store.PresignGetObject(ctx, bucket, key, time.Minute, "")
				if e != nil {
					t.Fatal("presign get failed")
				}
				perform("GET", get, "", 403)
				sdk := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("minioadmin", "minioadmin", "")}, func(o *s3.Options) {
					o.BaseEndpoint = aws.String("http://" + sharedMinIOEndpoint)
					o.UsePathStyle = true
				})
				head, e := s3.NewPresignClient(sdk).PresignHeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
				if e != nil {
					t.Fatal("presign HEAD failed")
				}
				perform("HEAD", strings.Replace(head.URL, "http://"+sharedMinIOEndpoint, base, 1), "", 403)
			}
			for _, kind := range []string{"users", "agents", "projects", "workspace_logo", "workspace_favicon"} {
				key := "avatars/" + kind + "/" + uuid.NewString() + "/" + uuid.NewString() + "/full.png"
				if e := store.PutObject(ctx, bucket, key, "image/png", []byte("trusted-variant")); e != nil {
					t.Fatal(e)
				}
				u, e := store.PresignGetObject(ctx, bucket, key, time.Minute, "")
				if e != nil {
					t.Fatal(e)
				}
				perform("GET", u, "", 200)
			}
			key := "tasks/" + uuid.NewString() + "/multipart"
			mu, e := store.InitiateMultipartUpload(ctx, bucket, key, "text/plain", 5<<20, 5<<20, time.Minute)
			if e != nil {
				t.Fatal(e)
			}
			etag := perform("PUT", mu.Parts[0].UploadURL, strings.Repeat("m", 5<<20), 200)
			if e = store.CompleteMultipartUpload(ctx, bucket, key, mu.UploadID, []storage.CompletedPart{{PartNumber: 1, ETag: etag}}); e != nil {
				t.Fatal(e)
			}
		})
	}
}
