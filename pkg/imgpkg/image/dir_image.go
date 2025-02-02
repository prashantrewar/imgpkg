// Copyright 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0

package image

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	regv1 "github.com/google/go-containerregistry/pkg/v1"
)

// Logger used to print messages
type Logger interface {
	Logf(msg string, args ...interface{})
}

type DirImage struct {
	dirPath     string
	img         regv1.Image
	shouldChown bool
	logger      Logger
}

// NewDirImage given an OCI Image representation creates a struct that will allow that image to be
// extracted into the provided directory
func NewDirImage(dirPath string, img regv1.Image, logger Logger) *DirImage {
	return &DirImage{dirPath, img, os.Getuid() == 0, logger}
}

// AsDirectory extracts the OCI image to the provided location in disk
func (i *DirImage) AsDirectory() error {
	err := os.RemoveAll(i.dirPath)
	if err != nil {
		return fmt.Errorf("Removing output directory: %s", err)
	}

	err = os.MkdirAll(i.dirPath, 0777)
	if err != nil {
		return fmt.Errorf("Creating output directory: %s", err)
	}

	layers, err := i.img.Layers()
	if err != nil {
		return err
	}

	fileMap := map[string]bool{}

	// we iterate through the layers in reverse order because it makes handling
	// whiteout layers more efficient, since we can just keep track of the removed
	// files as we see .wh. layers and ignore those in previous layers.
	for idx := len(layers) - 1; idx >= 0; idx-- {
		imgLayer := layers[idx]
		digest, err := imgLayer.Digest()
		if err != nil {
			return err
		}

		i.logger.Logf("Extracting layer '%s' (%d/%d)\n", digest, len(layers)-idx, len(layers))

		layerStream, err := imgLayer.Uncompressed()
		if err != nil {
			return err
		}

		defer layerStream.Close()

		err = i.writeLayer(fileMap, layerStream)
		if err != nil {
			return err
		}
	}

	return nil
}

// Taken from https://github.com/concourse/registry-image-resource/blob/b5481130ad61bc74e0a74f9b00b287b3a24bab88/cmd/in/unpack.go

func (i *DirImage) writeLayer(fileMap map[string]bool, stream io.Reader) error {
	tarReader := tar.NewReader(stream)

	for {
		hdr, err := tarReader.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		path := i.hydrateFilepath(hdr.Name)
		base := filepath.Base(path)

		const (
			whiteoutPrefix = ".wh."
		)

		if strings.HasPrefix(base, whiteoutPrefix) {
			dir := filepath.Dir(path)

			err := os.RemoveAll(filepath.Join(dir, strings.TrimPrefix(base, whiteoutPrefix)))
			if err != nil {
				return nil
			}
			fileMap[base] = true
			continue
		}

		// check for a whited out parent directory
		if inWhiteoutDir(fileMap, path) {
			continue
		}

		if fi, err := os.Lstat(path); err == nil {
			if fi.IsDir() && hdr.Name == "." {
				continue
			}
			if !(fi.IsDir() && hdr.Typeflag == tar.TypeDir) {
				if err := os.RemoveAll(path); err != nil {
					return err
				}
			}
		}

		fileMap[hdr.Name] = true
		err = i.extractTarEntry(hdr, tarReader)
		if err != nil {
			return err
		}
	}

	return nil
}

func inWhiteoutDir(fileMap map[string]bool, file string) bool {
	for {
		if file == "" {
			return false
		}
		dirname := filepath.Dir(file)
		if file == dirname {
			return false
		}
		if val, ok := fileMap[dirname]; ok && val {
			return true
		}
		file = dirname
	}
}

// Taken from https://github.com/concourse/go-archive/blob/f26802964d15194bddb07bf116ea567c56af973f/tarfs/extract.go

func (i *DirImage) extractTarEntry(header *tar.Header, input io.Reader) error {
	path := i.hydrateFilepath(header.Name)
	mode := header.FileInfo().Mode()

	// copy user permissions to group and other
	userPermission := int64(mode & 0700)
	permMode := os.FileMode(userPermission | userPermission>>3 | userPermission>>6)

	// By default, imgpkg will remove the permissions for group/all on all files.
	// Here we are checking if these permissions are still present. If this is the case it means that the creator
	// of the OCI image intended to keep the original permissions of the file. In this case we will honor the
	// request by keeping the original permissions on the files
	if mode&0077 > 0 {
		permMode = mode
	}

	err := os.MkdirAll(filepath.Dir(path), 0777)
	if err != nil {
		return err
	}

	switch header.Typeflag {
	case tar.TypeDir:
		return nil

	case tar.TypeReg, tar.TypeRegA:
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, permMode)
		if err != nil {
			return err
		}

		_, err = io.Copy(file, input)
		if err != nil {
			_ = file.Close()
			return err
		}

		err = file.Close()
		if err != nil {
			return err
		}

	case tar.TypeLink, tar.TypeSymlink:
		// skipping symlinks as a security feature
		return nil

	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		// skipping devices
		return nil

	default:
		return fmt.Errorf("Unsupported tar entry type '%c' for file '%s'", header.Typeflag, header.Name)
	}

	if runtime.GOOS != "windows" && i.shouldChown {
		err = os.Lchown(path, header.Uid, header.Gid)
		if err != nil {
			return err
		}
	}

	// must be done after everything
	return lchtimes(header, path)
}

func lchtimes(header *tar.Header, path string) error {
	aTime := header.AccessTime
	mTime := header.ModTime
	if aTime.Before(mTime) {
		aTime = mTime
	}

	if header.Typeflag == tar.TypeLink {
		if fi, err := os.Lstat(header.Linkname); err == nil && (fi.Mode()&os.ModeSymlink == 0) {
			return os.Chtimes(path, aTime, mTime)
		}
	} else if header.Typeflag != tar.TypeSymlink {
		return os.Chtimes(path, aTime, mTime)
	}

	return nil
}

// hydrateFilepath ensures that the file is correct based on the OS.
func (i *DirImage) hydrateFilepath(fPath string) string {
	var lPath string
	// We need to check the existance of \ type paths in the images because in previous versions of imgpkg images that
	// were created on Windows would have the path using \ instead of the new OS-agnostic version
	if strings.Contains(fPath, "\\") {
		lPath = filepath.Join(strings.Split(fPath, "\\")...)
	} else {
		lPath = filepath.Join(strings.Split(fPath, "/")...)
	}
	return filepath.Join(i.dirPath, lPath)
}
