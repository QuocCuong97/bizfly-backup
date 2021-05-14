package s3

import (
	"bytes"
	"log"
	"net/http"
	"time"

	"github.com/bizflycloud/bizfly-backup/pkg/volume"
)

type S3 struct {
	Name          string
	StorageBucket string
	SecretRef     string
	PresignURL    string
}

var _ volume.StorageVolume = (*S3)(nil)

func NewS3Default(name string, storageBucket string, secretRef string) *S3 {
	return &S3{
		Name:          name,
		StorageBucket: storageBucket,
		SecretRef:     secretRef,
	}
}

type HTTPClient struct{}

var (
	HttpClient = HTTPClient{}
)

var backoffSchedule = []time.Duration{
	1 * time.Second,
	3 * time.Second,
	10 * time.Second,
	20 * time.Second,
	30 * time.Second,
	40 * time.Second,
	60 * time.Second,
	80 * time.Second,
	100 * time.Second,
	120 * time.Second,
	3 * time.Minute,
	5 * time.Minute,
	10 * time.Minute,
	15 * time.Minute,
	20 * time.Minute,
	30 * time.Minute,
}

func putRequest(uri string, buf []byte) error {
	req, err := http.NewRequest("PUT", uri, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	log.Printf("PUT %s -> %d", req.URL, resp.StatusCode)

	defer resp.Body.Close()

	return nil
}

func (s3 *S3) PutObject(key string, data []byte) error {
	var err error
	for _, backoff := range backoffSchedule {
		err = putRequest(key, data)
		if err == nil {
			break
		}
		log.Printf("request error: %+v\n", err)
		log.Printf("retrying in %v\n", backoff)
		time.Sleep(backoff)
	}

	// all retries failed
	if err != nil {
		return err
	}

	return nil
}

func (s3 *S3) GetObject(key string) ([]byte, error) {
	panic("implement")
}

func (s3 *S3) HeadObject(key string) (int, error) {
	var resp *http.Response
	var err error
	for _, backoff := range backoffSchedule {
		resp, err = http.Head(key)
		if err == nil {
			break
		}
		log.Printf("request error: %+v\n", err)
		log.Printf("retrying in %v\n", backoff)
		time.Sleep(backoff)
	}

	// all retries failed
	if err != nil {
		log.Println(err)
	}

	return resp.StatusCode, nil
}

func (s3 *S3) SetCredential(preSign string) {
	panic("implement")
}