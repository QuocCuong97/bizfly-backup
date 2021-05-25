package backupapi

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/bizflycloud/bizfly-backup/pkg/progress"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

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
	ContentType  string `json:"content_type"`
	CreatedAt    string `json:"created_at"`
	Etag         string `json:"etag"`
	ID           string `json:"id"`
	ItemName     string `json:"item_name"`
	ItemType     string `json:"item_type"`
	LastModified string `json:"last_modified"`
	Mode         string `json:"mode"`
	RealName     string `json:"real_name"`
	Size         int    `json:"size"`
	Status       string `json:"status"`
	UpdatedAt    string `json:"updated_at"`
}

// FileResponse
type FilesResponse []File

// RecoveryPointResponse
type RecoveryPointResponse struct {
	Files []File `json:"files"`
	Total int    `json:"total"`
}

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

// InfoDownload
type InfoDownload struct {
	Get    string `json:"get"`
	Offset int    `json:"offset"`
}

// FileDownloadResponse
type FileDownloadResponse struct {
	Info []InfoDownload `json:"info"`
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

func (c *Client) SaveFilesInfo(recoveryPointID string, dir string, p *progress.Progress) (progress.Stat, FilesResponse, error) {

	reqURL, err := c.urlStringFromRelPath(c.saveFileInfoPath(recoveryPointID))
	if err != nil {
		return progress.Stat{}, FilesResponse{}, err
	}
	stat, filesInfo, err := WalkerDir(dir, p)
	if err != nil {
		return progress.Stat{}, FilesResponse{}, err
	}
	req, err := c.NewRequest(http.MethodPost, reqURL, filesInfo.Files)
	if err != nil {
		return progress.Stat{}, FilesResponse{}, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return progress.Stat{}, FilesResponse{}, err
	}

	defer resp.Body.Close()

	var files FilesResponse
	if err = json.NewDecoder(resp.Body).Decode(&files); err != nil {
		return progress.Stat{}, nil, err
	}

	return stat, files, nil
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

func (c *Client) UploadFile(recoveryPointID string, backupDir string, fi File, volume volume.StorageVolume, p *progress.Progress) error {
	p.Start()

	file, err := os.Open(filepath.Join(backupDir, fi.RealName))
	if err != nil {
		return err
	}

	chk := chunker.New(file, 0x3dea92648f6e83)
	buf := make([]byte, ChunkUploadLowerBound)

	s := progress.Stat{Files: 1}
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
		time.Sleep(500 * time.Millisecond)
		chunkResp, err := c.saveChunk(recoveryPointID, fi.ID, chunkReq)
		if err != nil {
			return err
		}

		resp, err := volume.HeadObject(chunkResp.PresignedURL.Head)
		if err != nil {
			return err
		}

		if etagHead, ok := resp.Header["Etag"]; ok {
			integrity := strings.Contains(etagHead[0], key)
			if !integrity {
				_, err := volume.PutObject(chunkResp.PresignedURL.Put, chunk.Data)
				if err != nil {
					return err
				}
			} else {
				log.Printf("exist key: %s, etag head: %s", key, etagHead)
			}
		} else {
			etagPut, err := volume.PutObject(chunkResp.PresignedURL.Put, chunk.Data)
			if err != nil {
				return err
			}
			integrity := strings.Contains(etagPut, key)
			if !integrity {
				_, err := volume.PutObject(chunkResp.PresignedURL.Put, chunk.Data)
				if err != nil {
					return err
				}
			}
		}
		s.Bytes = uint64(chunk.Length)
		p.Report(s)
		s.Files = 0
	}

	return nil
}

func (c *Client) RestoreFile(recoveryPointID string, destDir string, volume volume.StorageVolume, restoreSessionKey string, createdAt string) error {
	rp, err := c.GetListFilePath(recoveryPointID)
	if err != nil {
		return err
	}

	for _, f := range rp.Files {
		infos, err := c.GetInfoFileDownload(recoveryPointID, f.ID, restoreSessionKey, createdAt)
		if err != nil {
			return err
		}

		for _, info := range infos.Info {
			relativePathRealName := strings.Join(strings.Split(f.RealName, "/")[0:len(strings.Split(f.RealName, "/"))-1], "/")
			absolutePathRealName := filepath.Join(destDir, relativePathRealName)
			fileResore := filepath.Join(absolutePathRealName, filepath.Base(f.RealName))

			if err := EnsureDir(absolutePathRealName); err != nil {
				return err
			}

			file, err := CreateFile(fileResore)
			if err != nil {
				return err
			}
			defer file.Close()

			data, err := volume.GetObject(info.Get)
			if err != nil {
				return err
			}
			file.WriteAt(data, int64(info.Offset))
		}
	}

	return nil
}

func (c *Client) GetListFilePath(recoveryPointID string) (RecoveryPointResponse, error) {
	reqURL, err := c.urlStringFromRelPath(c.getListFilePath(recoveryPointID))
	if err != nil {
		return RecoveryPointResponse{}, err
	}

	req, err := c.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return RecoveryPointResponse{}, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return RecoveryPointResponse{}, err
	}
	var rp RecoveryPointResponse
	if err := json.NewDecoder(resp.Body).Decode(&rp); err != nil {
		return RecoveryPointResponse{}, err
	}

	return rp, nil
}

func (c *Client) GetInfoFileDownload(recoveryPointID string, itemID string, restoreSessionKey string, createdAt string) (FileDownloadResponse, error) {
	reqURL, err := c.urlStringFromRelPath(c.getInfoFileDownload(recoveryPointID, itemID))
	if err != nil {
		return FileDownloadResponse{}, err
	}

	req, err := c.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return FileDownloadResponse{}, err
	}
	req.Header.Add("X-Session-Created-At", createdAt)
	req.Header.Add("X-Restore-Session-Key", restoreSessionKey)

	resp, err := c.Do(req)
	if err != nil {
		return FileDownloadResponse{}, err
	}
	var fileDownload FileDownloadResponse
	if err := json.NewDecoder(resp.Body).Decode(&fileDownload); err != nil {
		return FileDownloadResponse{}, err
	}

	return fileDownload, nil
}

func WalkerDir(dir string, p *progress.Progress) (progress.Stat, FileInfoRequest, error) {
	p.Start()
	defer p.Done()

	var fileInfoRequest FileInfoRequest
	var stat progress.Stat
	err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		s := progress.Stat{}
		if !fi.IsDir() {
			singleFile := FileInfo{
				ItemName:     path,
				Size:         fi.Size(),
				LastModified: fi.ModTime().Format("2006-01-02 15:04:05.000000"),
				ItemType:     "FILE",
				Mode:         fi.Mode().Perm().String(),
			}
			fileInfoRequest.Files = append(fileInfoRequest.Files, singleFile)
			s.Files++
			s.Bytes += uint64(fi.Size())
		} else {
			s.Dirs++
		}
		p.Report(s)
		stat.Add(s)
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	return stat, fileInfoRequest, err
}

func EnsureDir(dirName string) error {
	err := os.MkdirAll(dirName, os.ModePerm)
	if err == nil || os.IsExist(err) {
		return nil
	} else {
		return err
	}
}

func CreateFile(name string) (*os.File, error) {
	file, err := os.Create(name)
	if err != nil {
		log.Println(err)
	}

	return file, nil
}
