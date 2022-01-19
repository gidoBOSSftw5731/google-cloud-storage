// Package mass_upload should upload content from a given ftp site
// to cloud storage. Verification of ftp site content against the
// cloud storage location before upload must be performed.
//
// Basic flow is:
//   1) start at the top of an FTP site.
//   2) download each file in turn, walking the remote directory tree.
//   3) calculate the md5() checksum for each file downloaded.
//   4) validate that the checksum matches the cloud-storage object's MD5 value.
//   5) if there is a mis-match, upload the ftp content to cloud-storage.
package main

import (
	"context"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"flag"
	"fmt"
	"io/ioutil"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/golang/glog"
	"github.com/jlaffaye/ftp"
	pb "github.com/routeviews/google-cloud-storage/proto/rv"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/idtoken"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/oauth"
)

const (
	dialTimeout = 5 * time.Second
	// Max message size set to 50mb.
	maxMsgSize = 50 * 1024 * 1024
	// Max ftp errors before exiting the process.
	maxFTPErrs = 50
)

var (
	// Google Cloud Storage bucket name to put content into.
	bucket = flag.String("bucket", "", "Bucket to mirror content into.")
	// Remote ftp archive URL to use as a starting point to read content from.
	archive = flag.String("archive", "", "Site URL to mirror content from: ftp://site/dir.")
	aUser   = flag.String("archive_user", "ftp", "Site userid to use with FTP.")
	aPasswd = flag.String("archive_pass", "mirror@", "Site password to use with this FTP.")

	// gRPC endpoint (https url) to upload replacement content to and credentials file, if necessary.
	grpcService   = flag.String("uploadURL", "rv-server-cgfq4yjmfa-uc.a.run.app:443", "Upload service host:port.")
	svcAccountKey = flag.String("saKey", "", "File location of service account key, if required.")
)

type client struct {
	gClient pb.RVClient
	bs      *storage.Client
	bh      *storage.BucketHandle
	fc      *ftp.ServerConn
	bucket  string
	// A channel which will contain
	ch chan *evalFile
	// Metrics, collect copied vs not for exit reporting.
	metrics map[string]int
}

type evalFile struct {
	// name is a full filename path:
	//   /bgpdata/route-views4/bgpdata/2022.01/UPDATES/updates.20220109.1830.bz2
	name string
	// chksum is an md5 checksum
	chksum string
}

func connectFtp(site string) (*ftp.ServerConn, error) {
	conn, err := ftp.Dial(site, ftp.DialWithTimeout(dialTimeout))
	return conn, err
}

// newGRPC makes a new grpc (over https) connection for the upload service.
// host is the hostname to connect to, saPath is a path to a stored json service account key.
func newGRPC(ctx context.Context, host, saPath string) (*grpc.ClientConn, error) {
	var opts []grpc.DialOption

	var idTokenSource oauth2.TokenSource
	var err error
	audience := "https://" + strings.Split(host, ":")[0]
	if saPath == "" {
		idTokenSource, err = idtoken.NewTokenSource(ctx, audience)
		if err != nil {
			if err.Error() != `idtoken: credential must be service_account, found "authorized_user"` {
				return nil, fmt.Errorf("idtoken.NewTokenSource: %v", err)
			}
			gts, err := google.DefaultTokenSource(ctx)
			if err != nil {
				return nil, fmt.Errorf("attempt to use Application Default Credentials failed: %v", err)
			}
			idTokenSource = gts
		}
	} else {
		idTokenSource, err = idtoken.NewTokenSource(ctx, audience, idtoken.WithCredentialsFile(saPath))
		if err != nil {
			return nil, fmt.Errorf("unable to create TokenSource: %v", err)
		}
	}

	opts = append(opts, grpc.WithAuthority(host))

	systemRoots, err := x509.SystemCertPool()
	if err != nil {
		return nil, err
	}
	cred := credentials.NewTLS(&tls.Config{
		RootCAs: systemRoots,
	})

	opts = append(opts,
		[]grpc.DialOption{
			grpc.WithTransportCredentials(cred),
			grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(maxMsgSize)),
			grpc.WithPerRPCCredentials(oauth.TokenSource{idTokenSource}),
		}...,
	)

	return grpc.Dial(host, opts...)
}

func new(ctx context.Context, aUser, aPasswd, site, bucket, grpcService, svcAccountKey string) (*client, error) {
	f, err := connectFtp(site)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to the ftp site(%v): %v", site, err)
	}

	// Login to cloud-storage, and get a bucket handle to the archive bucket.
	if err := f.Login(aUser, aPasswd); err != nil {
		return nil, fmt.Errorf("failed to login to site(%v) as u/p (%v/%v): %v",
			site, aUser, aPasswd, err)
	}

	c, errS := storage.NewClient(ctx)
	if errS != nil {
		return nil, fmt.Errorf("failed to create a new storage client: %v", errS)
	}
	// Get a BucketHandle, which enables access to the objects/etc.
	bh := c.Bucket(bucket)

	// Create a new upload service client.
	gc, err := newGRPC(ctx, grpcService, svcAccountKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC client: %v", err)
	}

	return &client{
		gClient: pb.NewRVClient(gc),
		bs:      c,
		bh:      bh,
		fc:      f,
		bucket:  bucket,
		ch:      make(chan *evalFile),
		metrics: map[string]int{"sync": 0, "skip": 0, "error": 0},
	}, nil
}

// close politely closes the handles to cloud-storage and the ftp archive.
func (c *client) close() {
	c.fc.Quit()
	if err := c.bs.Close(); err != nil {
		glog.Fatalf("failed to close the cloud-storage client: %v", err)
	}
}

// ftpWalk walks a defined directory, sending each file
// which matches a known pattern (updates) to a channel for further evaluation.
func (c *client) ftpWalk(dir string) {
	// Start the walk activity.
	w := c.fc.Walk(dir)

	// Walk the directory tree, stat/evaluate files, else continue walking.
	for w.Next() {
		glog.Infof("Eval Path: %s", w.Path())
		e := w.Stat()
		if e.Type == ftp.EntryTypeFolder && strings.HasSuffix(w.Path(), "RIBS") {
			glog.Info("Skipping RIBS directory")
			continue
		}
		if e.Type == ftp.EntryTypeFile && strings.HasPrefix(e.Name, "updates") {
			// Add the file to the channel, for evaluation and potential copy.
			glog.Infof("Sending file for eval: %s", w.Path())
			c.ch <- &evalFile{name: strings.TrimLeft(w.Path(), "/")}
		}
	}
	if w.Err() != nil {
		glog.Errorf("Next returned false, closing channel and returning: %v", w.Err())
		close(c.ch)
		return
	}
}

// readChannel reads FTP file results from a channel, collects and compares MD5 checksums
// and uploads files to cloud-storage if mismatches occur.
func (c *client) readChannel(ctx context.Context) {
	errs := 0
	for {
		ef := <-c.ch
		// Exit if ef is nil, because the channel closed.
		if ef == nil {
			glog.Error("Channel closed, exiting readChannel.")
			break
		}

		fn := strings.TrimLeft(ef.name, "/")
		csSum, err := c.getMD5cloud(ctx, fn)
		if err != nil {
			// If the object isn't there, it'll need to be uploaded.
			if strings.Contains(err.Error(), "object doesn't exist") {
				csSum = ""
			} else {
				// Any failure except 'does not exist', the cloud connection
				// is likely broken, fail and try restarting.
				glog.Fatalf("failed to get cloud md5 for file(%s): %v", fn, err)
			}
		}

		fSum, fc, err := c.getMD5ftp(ef.name)
		if err != nil {
			if errs < maxFTPErrs {
				glog.Infof("error getting md5(%s): %v", ef.name, err)
				errs++
				continue
			}
			// Enough failures have happened, exit and restart.
			glog.Fatalf("failed to get ftp md5 for file(%s): %v", ef.name, err)
		}

		if csSum == fSum {
			c.metrics["skip"]++
			continue
		}

		glog.Infof("Archiving file(%s) size(%d) to cloud.", ef.name, len(fc))
		req := pb.FileRequest{
			Filename: ef.name,
			Content:  fc,
			Md5Sum:   fSum,
			Project:  pb.FileRequest_ROUTEVIEWS,
		}
		resp, err := c.gClient.FileUpload(ctx, &req)
		if err != nil {
			glog.Errorf("failed uploading(%s) to grpcService: %v", ef.name, err)
			c.metrics["error"]++
			return
		}
		c.metrics["sync"]++
		glog.Infof("File upload status: %s", resp.GetStatus())
	}
}

func (c *client) getMD5cloud(ctx context.Context, path string) (string, error) {
	attrs, err := c.bh.Object(path).Attrs(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get attrs for obj: %v", err)
	}
	return hex.EncodeToString(attrs.MD5), nil
}

func (c *client) getMD5ftp(path string) (string, []byte, error) {
	r, err := c.fc.Retr(path)
	if err != nil {
		return "", nil, fmt.Errorf("failed to RETR the path: %v", err)
	}
	defer r.Close()

	buf, err := ioutil.ReadAll(r)
	return fmt.Sprintf("%x", md5.Sum(buf)), buf, nil
}

func main() {
	flag.Parse()
	if *bucket == "" || *archive == "" {
		glog.Fatal("set archive and bucket, or there is nothing to do")
	}

	// Clean up the archive (ftp://blah.org/floop/) to be a host/directory.
	var site, dir string
	site = *archive
	if strings.HasPrefix(site, "ftp://") {
		site = strings.TrimLeft(site, "ftp://")
	}
	if strings.HasSuffix(site, "/") {
		site = strings.TrimRight(site, "/")
	}
	parts := strings.Split(site, "/")
	site = parts[0] + ":21"
	dir = strings.Join(parts[1:], "/")
	dir = "/" + dir

	// Create a client, and start processing.
	// NOTE: Consider spawning N goroutines as fetch processors for the
	//       pathnames which are output from Walk().
	ctx := context.Background()
	c, err := new(ctx, *aUser, *aPasswd, site, *bucket, *grpcService, *svcAccountKey)
	if err != nil {
		glog.Fatalf("failed to create the client: %v", err)
	}

	// Start the FTP walk, then read from the channel and evaluate each file.
	go c.ftpWalk(dir)
	c.readChannel(ctx)

	// All operations ended, close the external services.
	glog.Info("Ending transmission/comparison.")
	c.close()
	fmt.Println("Metrics for file sync activity:")
	for k, v := range c.metrics {
		fmt.Printf("%s: %d\n", k, v)
	}
}
