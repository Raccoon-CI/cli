package main

import (
	"errors"
	"flag"
	"os"

	"raccoonci/internal/blob"
)

// cacheS3Flags put the actions cache's objects in an S3-compatible bucket
// instead of the controller's disk. Machines then download cache entries from
// the bucket directly (the links are presigned), which matters when the
// controller's own bandwidth is small: on InstaBox it is about 6 MB/s out, a
// machine downloads from a CDN at over 100 MB/s. Uploads still pass through
// the controller. The keys come from the environment, never from flags.
type cacheS3Flags struct {
	endpoint, bucket, region, prefix string
	pathStyle                        bool
}

func (f *cacheS3Flags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.bucket, "cache-s3-bucket", "", "keep cache entries in this S3 bucket instead of on this machine's disk (credentials in RACCOONCI_CACHE_S3_KEY and RACCOONCI_CACHE_S3_SECRET, optionally RACCOONCI_CACHE_S3_TOKEN)")
	fs.StringVar(&f.endpoint, "cache-s3-endpoint", "", "the S3-compatible store's address, for R2, MinIO, SeaweedFS and the like; empty means AWS")
	fs.StringVar(&f.region, "cache-s3-region", "", "the bucket's region (default us-east-1; R2 wants auto)")
	fs.StringVar(&f.prefix, "cache-s3-prefix", "raccoonci/", "put every object under this key prefix; each repository gets its own directory below it")
	fs.BoolVar(&f.pathStyle, "cache-s3-path-style", false, "address the bucket as host/bucket/key (self-hosted stores need it)")
}

// store returns the bucket store for one repository, or nil when no bucket
// was asked for.
func (f *cacheS3Flags) store(repoDir string) (blob.Store, error) {
	if f.bucket == "" {
		return nil, nil
	}
	key, secret := os.Getenv("RACCOONCI_CACHE_S3_KEY"), os.Getenv("RACCOONCI_CACHE_S3_SECRET")
	if key == "" || secret == "" {
		return nil, errors.New("-cache-s3-bucket needs RACCOONCI_CACHE_S3_KEY and RACCOONCI_CACHE_S3_SECRET in the environment")
	}
	return blob.NewS3(blob.S3Config{
		Endpoint: f.endpoint, Region: f.region, Bucket: f.bucket,
		AccessKey: key, SecretKey: secret, SessionToken: os.Getenv("RACCOONCI_CACHE_S3_TOKEN"),
		PathStyle: f.pathStyle, Prefix: f.prefix + repoDir + "/",
	})
}
