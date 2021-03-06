package backupapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"sync"

	"github.com/hashicorp/go-retryablehttp"
)

const MultipartUploadLowerBound = 15 * 1000 * 1000

// File ...
type File struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Size        int    `json:"size"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	ContentType string `json:"content_type"`
	Etag        string `json:"eTag"`
}

// Multipart ...
type Multipart struct {
	UploadID string `json:"upload_id"`
	FileName string `json:"file_name"`
}

// Part ...
type Part struct {
	PartNumber int    `json:"part_number"`
	Size       int    `json:"size"`
	Etag       string `json:"etag"`
}

func (c *Client) uploadFilePath(recoveryPointID string) string {
	return fmt.Sprintf("/agent/recovery-points/%s/file", recoveryPointID)
}

func (c *Client) urlStringFromRelPath(relPath string) (string, error) {
	if c.ServerURL.Path != "" && c.ServerURL.Path != "/" {
		relPath = path.Join(c.ServerURL.Path, relPath)
	}
	relURL, err := url.Parse(relPath)
	if err != nil {
		return "", err
	}

	u := c.ServerURL.ResolveReference(relURL)
	return u.String(), nil
}

func (c *Client) uploadFile(fn string, r io.Reader, pw io.Writer) error {
	bodyBuf := &bytes.Buffer{}
	bodyWriter := multipart.NewWriter(bodyBuf)
	fileWriter, err := bodyWriter.CreateFormFile("data", fn)
	if err != nil {
		return fmt.Errorf("bodyWriter.CreateFormFile: %w", err)
	}

	_, err = io.Copy(fileWriter, r)
	if err != nil {
		return err
	}

	contentType := bodyWriter.FormDataContentType()
	if err := bodyWriter.Close(); err != nil {
		return err
	}

	reqURL, err := c.urlStringFromRelPath(c.uploadFilePath(fn))
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, reqURL, io.TeeReader(bodyBuf, pw))
	if err != nil {
		return err
	}

	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = 50 // Should configurable this?
	resp, err := c.do(retryClient.StandardClient(), req, contentType)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	_, err = io.Copy(ioutil.Discard, resp.Body)
	return err
}

func (c *Client) uploadMultipart(recoveryPointID string, r io.Reader, pw io.Writer) error {
	ctx := context.Background()
	m, err := c.InitMultipart(ctx, recoveryPointID)
	if err != nil {
		return err
	}

	bufCh := make(chan []byte, 30)
	go func() {
		defer close(bufCh)
		b := make([]byte, MultipartUploadLowerBound)
		for {
			n, err := r.Read(b)
			if err != nil {
				return
			}
			bufCh <- b[:n]
		}
	}()

	partNum := 0
	var wg sync.WaitGroup
	var errs []error
	var mu sync.Mutex
	sem := make(chan struct{}, 15)
	rc := retryablehttp.NewClient()
	rc.RetryMax = 50 // TODO: configurable?
	rcStd := rc.StandardClient()
	for buf := range bufCh {
		sem <- struct{}{}
		buf := buf
		partNum++
		wg.Add(1)
		go func(buf []byte, partNum int) {
			defer func() {
				<-sem
				wg.Done()
			}()
			b := new(bytes.Buffer)
			bodyWriter := multipart.NewWriter(b)
			fileWriter, err := bodyWriter.CreateFormFile("data", recoveryPointID+"-"+strconv.Itoa(partNum))
			if err != nil {
				return
			}
			_, _ = fileWriter.Write(buf)
			contentType := bodyWriter.FormDataContentType()
			if err := bodyWriter.Close(); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}

			reqURL, err := c.urlStringFromRelPath(c.uploadPartPath(recoveryPointID))
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			req, err := http.NewRequest(http.MethodPut, reqURL, io.TeeReader(b, pw))
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			q := req.URL.Query()
			q.Add("part_number", strconv.Itoa(partNum))
			q.Add("upload_id", m.UploadID)
			req.URL.RawQuery = q.Encode()

			resp, err := c.do(rcStd, req, contentType)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			defer resp.Body.Close()

			if _, err := io.Copy(ioutil.Discard, resp.Body); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(buf, partNum)
	}
	wg.Wait()

	if len(errs) > 0 {
		return fmt.Errorf("upload multiparts fails: %v", errs)
	}
	rc.HTTPClient.CloseIdleConnections()

	return c.CompleteMultipart(ctx, recoveryPointID, m.UploadID)
}

// UploadFile uploads given file to server.
func (c *Client) UploadFile(fn string, r io.Reader, pw io.Writer, batch bool) error {
	if batch {
		return c.uploadMultipart(fn, r, pw)

	}
	return c.uploadFile(fn, r, pw)
}
