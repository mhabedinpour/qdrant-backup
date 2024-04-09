package main

import (
	"compress/gzip"
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/cenkalti/backoff/v4"
	pb "github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
)

const (
	BackoffMaxInterval    = 30 * time.Second
	BackoffInitialFactor  = 250
	BackoffMaxElapsedTime = 60 * time.Second
)

func newBackoff() *backoff.ExponentialBackOff {
	b := &backoff.ExponentialBackOff{
		InitialInterval:     BackoffInitialFactor,
		RandomizationFactor: backoff.DefaultRandomizationFactor,
		Multiplier:          backoff.DefaultMultiplier,
		MaxInterval:         BackoffMaxInterval,
		MaxElapsedTime:      BackoffMaxElapsedTime,
		Stop:                backoff.Stop,
		Clock:               backoff.SystemClock,
	}
	b.Reset()

	return b
}

type Node struct {
	client      *grpc.ClientConn
	name        string
	restAddress string
	apiKey      string
}

func getNodes(serviceAddress string, grpcPort string, httpPort string, apiKey string) ([]Node, error) {
	ips, err := net.LookupIP(serviceAddress)
	if err != nil {
		return nil, fmt.Errorf("could not resolve ips: %w", err)
	}

	result := make([]Node, 0, len(ips))
	for _, ip := range ips {
		address := ip.To4()
		if address == nil {
			continue
		}

		conn, err := grpc.NewClient(fmt.Sprintf("%s:%s", address, grpcPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, fmt.Errorf("could not create grpc client for %s: %w", address, err)
		}

		hostnames, err := net.LookupAddr(address.String())
		if err != nil {
			return nil, fmt.Errorf("could not resolve hostname for %s: %w", address, err)
		}

		hostname := ""
		for _, name := range hostnames {
			if strings.Contains(name, serviceAddress) {
				hostname = name

				break
			}
		}
		if len(hostnames) == 0 {
			return nil, fmt.Errorf("got empty hostname for %s", address)
		}

		result = append(result, Node{
			client:      conn,
			name:        strings.Split(hostname, ".")[0],
			restAddress: fmt.Sprintf("http://%s:%s", address, httpPort),
			apiKey:      apiKey,
		})
	}

	return result, nil
}

func createSnapshot(ctx context.Context, node Node, collectionName string) (string, error) {
	snapshotClient := pb.NewSnapshotsClient(node.client)

	snapshot, err := snapshotClient.Create(ctx, &pb.CreateSnapshotRequest{CollectionName: collectionName})
	if err != nil {
		return "", fmt.Errorf("could not create snapshot: %w", err)
	}

	return snapshot.SnapshotDescription.Name, nil
}

func removeSnapshots(ctx context.Context, node Node, collectionName string) error {
	snapshotClient := pb.NewSnapshotsClient(node.client)

	snapshots, err := snapshotClient.List(ctx, &pb.ListSnapshotsRequest{CollectionName: collectionName})
	if err != nil {
		return fmt.Errorf("could not list full snapshots: %w", err)
	}

	for _, snapshot := range snapshots.GetSnapshotDescriptions() {
		_, err := snapshotClient.Delete(ctx, &pb.DeleteSnapshotRequest{CollectionName: collectionName, SnapshotName: snapshot.Name})
		if err != nil {
			return fmt.Errorf("could not delete snapshot %s: %w", snapshot.Name, err)
		}
	}

	return nil
}

func uploadSnapshot(ctx context.Context, node Node, collectionName string, snapshotName string, uploader *s3manager.Uploader, bucketName string, keyPrefix string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/collections/%s/snapshots/%s", node.restAddress, collectionName, snapshotName), nil)
	if err != nil {
		return fmt.Errorf("could not create request: %w", err)
	}

	req.Header.Set("api-key", node.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not download snapshot: %w", err)
	}

	pipeReader, pipeWriter := io.Pipe()
	gzWriter := gzip.NewWriter(pipeWriter)
	defer func() {
		_ = pipeReader.Close()
	}()

	go func() {
		defer func() {
			_ = resp.Body.Close()
		}()

		_, err = io.Copy(gzWriter, resp.Body)
		if err != nil {
			_ = gzWriter.Close()
			_ = pipeWriter.CloseWithError(err)

			return
		}

		_ = pipeWriter.CloseWithError(gzWriter.Close())
	}()

	_, err = uploader.UploadWithContext(ctx, &s3manager.UploadInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(fmt.Sprintf("%s/%s/%s.snapshot.gz", keyPrefix, collectionName, node.name)),
		Body:   pipeReader,
	})
	if err != nil {
		return fmt.Errorf("could not upload file: %w", err)
	}

	return nil
}

func backupNode(ctx context.Context, node Node, collectionName string, uploader *s3manager.Uploader, bucketName string, uploadKeyPrefix string) error {
	fmt.Printf("removing snapshots of %s-%s to free-up disk space\n", collectionName, node.name)
	err := removeSnapshots(ctx, node, collectionName)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "could not remove snapshots of %s-%s: %v\n", collectionName, node.name, err)

		return err
	}

	fmt.Printf("creating snapshot of %s-%s\n", collectionName, node.name)
	snapshotName, err := createSnapshot(ctx, node, collectionName)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "could not create snapshot %s-%s: %v\n", collectionName, node.name, err)

		return err
	}

	fmt.Printf("uploading snapshot of %s-%s\n", collectionName, node.name)
	err = uploadSnapshot(ctx, node, collectionName, snapshotName, uploader, bucketName, uploadKeyPrefix)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "could not upload snapshot of %s-%s: %v\n", collectionName, node.name, err)

		return err
	}

	fmt.Printf("cleaning up %s-%s\n", collectionName, node.name)
	err = removeSnapshots(ctx, node, collectionName)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "could not cleanup %s-%s: %v\n", collectionName, node.name, err)

		// at this point let's ignore the error since the backup is actually done.
	}

	return nil
}

func main() {
	qdrantGrpcServiceAddress := os.Getenv("QDRANT_GRPC_SERVICE_ADDRESS")
	qdrantGrpcPort := os.Getenv("QDRANT_GRPC_PORT")
	qdrantHttpPort := os.Getenv("QDRANT_HTTP_PORT")
	qdrantApiKey := os.Getenv("QDRANT_TOKEN")

	objectStorageAccessKey := os.Getenv("OBJECT_STORAGE_ACCESS_KEY")
	objectStorageAccessSecret := os.Getenv("OBJECT_STORAGE_ACCESS_SECRET")
	objectStorageBucketName := os.Getenv("OBJECT_STORAGE_BUCKET_NAME")
	objectStorageAddress := os.Getenv("OBJECT_STORAGE_ADDRESS")
	objectStorageRegion := os.Getenv("OBJECT_STORAGE_REGION")
	s3Session := session.Must(session.NewSession(&aws.Config{
		Endpoint:    &objectStorageAddress,
		Region:      &objectStorageRegion,
		Credentials: credentials.NewStaticCredentials(objectStorageAccessKey, objectStorageAccessSecret, ""),
	}))
	uploader := s3manager.NewUploader(s3Session)
	uploadKeyPrefix := time.Now().Format("2006-01-02T150405")

	collections := strings.Split(os.Getenv("COLLECTIONS_TO_BACKUP"), ",")

	nodes, err := getNodes(qdrantGrpcServiceAddress, qdrantGrpcPort, qdrantHttpPort, qdrantApiKey)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Could not get nodes: %v\n", err)
		os.Exit(1)
	}

	wg := sync.WaitGroup{}
	ctx := context.Background()
	successes := 0
	total := len(collections) * len(nodes)
	for _, collectionName := range collections {
		for _, node := range nodes {
			wg.Add(1)

			go func() {
				defer wg.Done()

				md := metadata.New(map[string]string{"api-key": qdrantApiKey})
				ctx = metadata.NewOutgoingContext(ctx, md)

				b := newBackoff()
				err := backoff.Retry(func() error {
					return backupNode(ctx, node, collectionName, uploader, objectStorageBucketName, uploadKeyPrefix)
				}, b)
				if err != nil {
					_, _ = fmt.Fprintf(os.Stderr, "could not backup %s-%s after all retries: %v\n", collectionName, node.name, err)
				} else {
					successes++
				}
			}()
		}
	}

	wg.Wait()
	fmt.Printf("finished, Successes: %d, Total: %d\n", successes, total)

	// exit as failure if couldn't back up everything.
	if total > successes {
		os.Exit(1)
	}
}
