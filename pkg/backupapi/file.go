package backupapi

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"

	"github.com/bizflycloud/bizfly-backup/pkg/volume"
	"github.com/restic/chunker"
)

const ChunkUploadLowerBound = 15 * 1000 * 1000

type FileInfo struct {
	ItemName     string `json:"item_name"`
	Size         int64  `json:"size"`
	ItemType     string `json:"item_type"`
	Mode         string `json:"mode"`
	LastModified string `json:"last_modified"`
}

type FileInfoRequest struct {
	Files []FileInfo `json:"files"`
}

// File ...
type File struct {
	ID          string `json:"id"`
	Name        string `json:"item_name"`
	Size        int    `json:"size"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	ContentType string `json:"content_type"`
	Etag        string `json:"eTag"`
	RealName    string `json:"real_name"`
}

// FileResponse
type FilesResponse []File

// ChunkRequest
type ChunkRequest struct {
	Length    uint   `json:"length"`
	Offset    uint   `json:"offset"`
	HexSha256 string `json:"hex_sha256"`
}

// ChunkResponse
type ChunkResponse struct {
	ID           string `json:"id"`
	Offset       uint   `json:"offset"`
	Length       uint   `json:"length"`
	HexSha256    string `json:"hex_sha256"`
	Uri          string `json:"uri"`
	PresignedURL struct {
		Head string `json:"head"`
		Put  string `json:"put"`
	} `json:"presigned_url"`
}

func (c *Client) saveFileInfoPath(recoveryPointID string) string {
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

func (c *Client) SaveFilesInfo(recoveryPointID string, dir string) (FilesResponse, error) {
	reqURL, err := c.urlStringFromRelPath(c.saveFileInfoPath(recoveryPointID))
	if err != nil {
		return FilesResponse{}, err
	}
	filesInfo, err := WalkerDir(dir)
	if err != nil {
		return FilesResponse{}, err
	}
	req, err := c.NewRequest(http.MethodPost, reqURL, filesInfo.Files)
	if err != nil {
		return FilesResponse{}, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return FilesResponse{}, err
	}

	defer resp.Body.Close()

	var files FilesResponse
	if err = json.NewDecoder(resp.Body).Decode(&files); err != nil {
		return nil, err
	}

	return files, nil
}

func (c *Client) saveChunk(recoveryPointID string, fileID string, chunk ChunkRequest) (ChunkResponse, error) {
	reqURL, err := c.urlStringFromRelPath(c.saveChunkPath(recoveryPointID, fileID))
	if err != nil {
		return ChunkResponse{}, err
	}

	req, err := c.NewRequest(http.MethodPost, reqURL, chunk)
	if err != nil {
		return ChunkResponse{}, err
	}

	resp, err := c.Do(req)
	if err != nil {
		return ChunkResponse{}, err
	}
	defer resp.Body.Close()

	var chunkResp ChunkResponse

	if err := json.NewDecoder(resp.Body).Decode(&chunkResp); err != nil {
		return ChunkResponse{}, err
	}

	return chunkResp, nil
}

func (c *Client) UploadFile(recoveryPointID string, backupDir string, fi File, volume volume.StorageVolume) error {
	file, err := os.Open(filepath.Join(backupDir, fi.RealName))
	if err != nil {
		return err
	}
	chk := chunker.New(file, 0x3dea92648f6e83)
	buf := make([]byte, ChunkUploadLowerBound)

	for {
		chunk, err := chk.Next(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		hash := md5.Sum(chunk.Data)
		key := hex.EncodeToString(hash[:])
		chunkReq := ChunkRequest{
			Length:    chunk.Length,
			Offset:    chunk.Start,
			HexSha256: key,
		}
		chunkResp, err := c.saveChunk(recoveryPointID, fi.ID, chunkReq)
		if err != nil {
			return err
		}
		log.Printf("chunk info %d\t%d\t%016x\t%032x\n", chunk.Start, chunk.Length, chunk.Cut, hash)

		statusCode, err := volume.HeadObject(chunkResp.PresignedURL.Head)
		if err != nil {
			return err
		}
		if statusCode != 200 {
			err = volume.PutObject(chunkResp.PresignedURL.Put, chunk.Data)
			if err != nil {
				return err
			}
		} else {
			log.Printf("exists object, key: %s", key)
		}
	}

	return nil
}

func WalkerDir(dir string) (FileInfoRequest, error) {
	var fileInfoRequest FileInfoRequest

	err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			singleFile := FileInfo{
				ItemName:     path,
				Size:         fi.Size(),
				LastModified: fi.ModTime().Format("2006-01-02 15:04:05.000000"),
				ItemType:     "FILE",
				// Mode:         fileInfo.Mode().Perm().String(),
				Mode: "0123",
			}
			fileInfoRequest.Files = append(fileInfoRequest.Files, singleFile)
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	return fileInfoRequest, err
}
