// Package main is the grpc server which accepts file storage requests,
//
// Files are stored in cloud-storage, they may be converted and added to
// BigQuery tables as well, depending upon the request.
//
// NOTE: Currently (12/2021) all files are stored to GCS, the conversion process
//       subscribes to a pubsub feed of buckets which are to be converted for BigQuery.
package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"

	"cloud.google.com/go/storage"
	"github.com/golang/glog"
	log "github.com/golang/glog"
	converter "github.com/routeviews/google-cloud-storage/pkg/mrt_converter"
	pb "github.com/routeviews/google-cloud-storage/proto/rv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

const (
	// https://cloud.google.com/storage/docs/reference/libraries#client-libraries-install-go
	// TODO(morrowc): Sort out organization privilege problems to create a service account key.
	// Be sure to have the JSON authentication bits in env(GOOGLE_APPLICATION_CREDENTIALS)
	projectID = "1071922449970"

	// Set a max receive message size: 50mb
	maxMsgSize = 50 * 1024 * 1024
)

var (
	port   = os.Getenv("PORT")
	bucket = flag.String("bucket", "routeviews-archives", "Cloud storage bucket name.")

	// TODO(morrowc): find a method to define the TLS certificate to be used, if this will
	//                not be done through GCLB's inbound https path.
)

type rvServer struct {
	bucket string
	sc     *storage.Client
	pb.UnimplementedRVServer
}

// setProjectMeta set project source in the metadata of a GCS object. The
// object must've existed when we set metadata.
func (r rvServer) setProjectMeta(ctx context.Context, obj string, proj pb.FileRequest_Project) error {
	// Set metadata once the object is created.
	if _, err := r.sc.Bucket(r.bucket).Object(obj).Update(ctx, storage.ObjectAttrsToUpdate{
		Metadata: map[string]string{
			converter.ProjectMetadataKey: proj.String(),
		},
	}); err != nil {
		return fmt.Errorf("failed to set metadata '%s:%s': %v", converter.ProjectMetadataKey, proj.String(), err)
	}
	glog.Infof("Set metadata for object: %s", obj)
	return nil
}

// fileStore stores a file ([]byte) to a designated bucket location (string).
func (r rvServer) fileStore(ctx context.Context, fn string, b []byte) error {
	// Store the file content to the destination bucket.
	wc := r.sc.Bucket(r.bucket).Object(fn).NewWriter(ctx)
	defer wc.Close()
	if _, err := io.Copy(wc, bytes.NewReader(b)); err != nil {
		return fmt.Errorf("failed copying content to destination: %s/%s: %v", r.bucket, fn, err)
	}
	glog.Infof("Stored object to GCS: %s", fn)
	return nil
}

// newRVServer creates and returns a proper RV object.
func newRVServer(bucket string, client *storage.Client) (rvServer, error) {
	return rvServer{
		bucket: bucket,
		sc:     client,
	}, nil
}

// Store a RARC RPKI or Routeviews file to cloud storage.
func (r rvServer) handleDataFile(ctx context.Context, proj pb.FileRequest_Project, resp *pb.FileResponse, fn string, c []byte) (*pb.FileResponse, error) {
	if err := r.fileStore(ctx, fn, c); err != nil {
		resp.Status = pb.FileResponse_FAIL
		return resp, err
	}
	if err := r.setProjectMeta(ctx, fn, proj); err != nil {
		resp.Status = pb.FileResponse_FAIL
		return resp, err
	}
	resp.Status = pb.FileResponse_SUCCESS

	glog.Infof("Finished processing datafile: %s", fn)
	return resp, nil
}

// FileUpload collects a file and handles it according to the appropriate rules.
//  FileRequeasts must have:
//    filename
//    checksum
//    content
//    project
//
// If any of these is missing the requset is invalid.
//
func (r rvServer) FileUpload(ctx context.Context, req *pb.FileRequest) (*pb.FileResponse, error) {
	resp := &pb.FileResponse{}

	fn := req.GetFilename()
	content := req.GetContent()
	proj := req.GetProject()
	sum := req.GetMd5Sum()
	if len(content) < 1 || proj == pb.FileRequest_UNKNOWN || len(fn) < 1 {
		resp.Status = pb.FileResponse_FAIL
		return nil, errors.New("base requirements for FileRequest unmet")
	}

	// validate that content checksum matches the requseted checksum.
	ts := md5.Sum(content)
	tsString := hex.EncodeToString(ts[:])
	if tsString != sum {
		resp.Status = pb.FileResponse_FAIL
		return nil, fmt.Errorf("checksum failure req(%q) != calc(%q)", sum, tsString)
	}

	// Process the content based upon project requirements.
	switch {
	case proj == pb.FileRequest_ROUTEVIEWS:
		return r.handleDataFile(ctx, pb.FileRequest_ROUTEVIEWS, resp, fn, content)
	case proj == pb.FileRequest_RIPE_RIS:
	case proj == pb.FileRequest_RPKI_RARC:
		// Simply store the file.
		return r.handleDataFile(ctx, pb.FileRequest_RPKI_RARC, resp, fn, content)
	}

	return nil, fmt.Errorf("not Implemented storing: %v", req.GetFilename())
}

func main() {
	flag.Parse()

	if port == "" {
		port = "9876"
	}
	log.Infof("Service will listren on port : %s", port)

	// Start the listener.
	// NOTE: this listens on all IP Addresses, caution when testing.
	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		log.Fatalf("failed to listen(): %v", err)
	}

	// Create a storage client, to add to the RV Server.
	c, err := storage.NewClient(context.Background())
	if err != nil {
		log.Fatalf("failed to create storage client: %v", err)
	}

	r, err := newRVServer(*bucket, c)
	if err != nil {
		log.Fatalf("failed to create new rvServer: %v", err)
	}

	s := grpc.NewServer(
		grpc.MaxMsgSize(maxMsgSize),
		grpc.MaxRecvMsgSize(maxMsgSize),
		grpc.MaxSendMsgSize(maxMsgSize),
	)
	pb.RegisterRVServer(s, r)

	// Register the reflection service on gRPC server.
	reflection.Register(s)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to listen&&serve: %v", err)
	}

}