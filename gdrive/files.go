// Package gdrive is a package which provides utilities for manipulating (list,
// search, upload, download, delete) files on Google Drive.
// It has special handling for .csv files which it uploads as a Google Sheets
// documents (and downloads the first visible sheet of Google Sheets documents
// as .csv text) This can be more simple than using Google's API for common
// tasks; for anything more complicated use Google's golang sdk directly:
// https://pkg.go.dev/google.golang.org/api/drive/v3
package gdrive

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

// Map google doc type to MIME
// https://developers.google.com/drive/api/v3/ref-export-formats
var exportMap = map[string]string{
	"spreadsheet": "text/csv",
	"drawing":     "text/svg",
}

// Define an interface so we can mock the FilesService type for testing if we
// need to
type driveFiler interface {
	Create(file *drive.File) *drive.FilesCreateCall
	Delete(fileId string) *drive.FilesDeleteCall
	List() *drive.FilesListCall
	Get(fileId string) *drive.FilesGetCall
	Export(fileId string, mimeType string) *drive.FilesExportCall
	Update(fileId string, file *drive.File) *drive.FilesUpdateCall
}

// Service wraps drive.FilesService
type Service struct {
	ctx   context.Context
	filer driveFiler
}

// NewServiceWithCtx creates and wraps a new FilesService with the provided
// context
func NewServiceWithCtx(ctx context.Context) (*Service, error) {
	gsvc, err := drive.NewService(ctx)
	if err != nil {
		return nil, err
	}
	return &Service{
		ctx:   ctx,
		filer: gsvc.Files,
	}, nil
}

func (svc *Service) WithService(service *drive.Service) {
	svc.filer = service.Files
}

// FilesService returns a pointer to the wrapped FilesService
func (svc *Service) FilesService() *drive.FilesService {
	return svc.filer.(*drive.FilesService)
}

// Search searches all of the authenticated user's files.
// 'q' is the search query as documented here:
// https://developers.google.com/drive/api/v3/ref-search-terms
func (svc *Service) Search(q string) ([]*drive.File, error) {

	// closure used to iterate pages and collect all files
	var files []*drive.File
	pages := func(fl *drive.FileList) error {
		files = append(files, fl.Files...)
		return nil
	}

	listCall := svc.filer.List().Fields("files(id, name, parents, shared)").SupportsAllDrives(true).IncludeTeamDriveItems(true).Q(q)
	err := listCall.Pages(svc.ctx, pages)
	if err == nil {
		_, err = listCall.Do()
	}
	return files, err
}

// FilesNamed returns a list of all files named 'name' in the 'parent' folder.
// If parent is empty, will return files from any files shared with user.
// If no matching file is found, returns empty list and nil error
// If an error is encountered it is returned along with any files that were
// found before encountering the error
func (svc *Service) FilesNamed(name, parent string) ([]*drive.File, error) {
	query := fmt.Sprintf("name = '%s'", escapeQuery(name))
	if parent != "" {
		query += fmt.Sprintf(" and '%s' in parents", parent)
	}
	return svc.Search(query)
}

func escapeQuery(q string) string {
	q = strings.ReplaceAll(q, `\`, `\\`)
	return strings.ReplaceAll(q, `'`, `\'`)
}

// CreateFolder creates a new empty directory named 'name' in folder with id
// 'parent'.
// If parent is empty, directory will be created in the authenticated user's
// drive root.
// (This will not overwrite any other files with the same name.)
// https://developers.google.com/drive/api/v3/folder
func (svc *Service) CreateFolder(name, parent string) (*drive.File, error) {
	createCall := svc.filer.Create(&drive.File{
		Name:     name,
		MimeType: "application/vnd.google-apps.folder",
		Parents:  []string{parent},
	})
	return createCall.Do()
}

// On Windows the mime.TypeByExtension method can return wrong values
// (https://github.com/golang/go/issues/32350)
// So we hardcode the most important extension(s)
func typeByExtension(ext string) string {
	ext = strings.ToLower(ext)
	if ext == ".csv" {
		return "text/csv; charset=utf-8"
	}
	return mime.TypeByExtension(ext)
}

// CreateFile creats a new file named 'name' in folder with id 'parent' and
// content read from 'src'.
// If name has '.csv' extension, then the created file is converted to a Google
// Sheets document on the drive.
// If parent is empty, file will be created in user's drive root.
// If 'src' is nil, creates an empty file.
// (This will not overwrite any other files with the same name.)
// https://developers.google.com/drive/api/v3/create-file
func (svc *Service) CreateFile(name, parent string, src io.Reader) (*drive.File, error) {
	ext := filepath.Ext(name)
	mime := typeByExtension(ext)
	var gmime string
	if strings.Contains(mime, "text/csv") {
		gmime = "application/vnd.google-apps.spreadsheet"
	}
	createCall := svc.filer.Create(&drive.File{
		Name:     name,
		MimeType: gmime,
		Parents:  []string{parent},
	})
	if src != nil {
		createCall.Media(src, googleapi.ContentType(mime))
	}
	return createCall.Do()
}

// CreateOrUpdateFile creates a new file named 'name' (with contents of 'src')
// if it does not already exist in 'parent'; otherwise it replaces the contents
// of the existing file.
func (svc *Service) CreateOrUpdateFile(name, parent string,
	src io.Reader) (*drive.File, error) {
	var file *drive.File

	files, err := svc.FilesNamed(name, parent)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(name, ".csv") &&
		(files != nil || len(files) == 0) {
		// Try again without the extension
		// This is because when Google Drive imports .csv files it strips the
		// ext from the file name, so searching for the same name to update the
		// file will end up just creating a new file and so on.
		var name = strings.TrimSuffix(name, ".csv")
		files, err = svc.FilesNamed(name, parent)
		if err != nil {
			return nil, err
		}
	}

	if len(files) > 0 {
		file, err = svc.UpdateFile(files[0].Id, name, src)
	} else {
		file, err = svc.CreateFile(name, parent, src)
	}
	return file, err
}

// UpdateFile replaces an existing drive file (id) the contents read from 'src'
// and updates its name to 'name'
func (svc *Service) UpdateFile(id, name string, src io.Reader) (*drive.File, error) {
	updateCall := svc.filer.Update(id, &drive.File{})
	if src != nil {
		ext := filepath.Ext(name)
		updateCall.Media(src, googleapi.ContentType(typeByExtension(ext)))
	}
	return updateCall.Do()
}

// GetInfo returns all metadata for the file identified by 'id'
func (svc *Service) GetInfo(id string) (*drive.File, error) {
	return svc.filer.Get(id).Fields("*").Do()
}

// DownloadFile returns a http.Response for downloading the contents of file
// identified by 'id'.
// If file is a Google Workspace file it is exported as a text format.
// https://developers.google.com/drive/api/v3/manage-downloads
func (svc *Service) DownloadFile(id string) (*http.Response, error) {
	var dlFunc func(...googleapi.CallOption) (*http.Response, error)
	getCall := svc.filer.Get(id)
	file, err := getCall.Do()
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(file.MimeType, "application/vnd.google-apps") {
		// it is a google workspace doc we must export
		parts := strings.Split(file.MimeType, ".")
		driveType := parts[len(parts)-1]
		mime := exportMap[driveType]
		if mime == "" {
			mime = "text/plain"
		}
		dlFunc = svc.filer.Export(id, mime).Download
	} else {
		// we can download this file
		dlFunc = getCall.Download
	}
	return dlFunc()
}

// FileContents downloads and returns the contents of the file identified by
// 'id'
func (svc *Service) FileContents(id string) ([]byte, error) {
	resp, err := svc.DownloadFile(id)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return ioutil.ReadAll(resp.Body)
}

// DeleteFile deletes file identified by 'id'
func (svc *Service) DeleteFile(id string) error {
	return svc.filer.Delete(id).Do()
}
