package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
)

// skipDirs are never uploaded: dependencies, build output, VCS and tool state.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true, ".next": true, ".nuxt": true,
	".svelte-kit": true, ".turbo": true, ".cache": true, ".venv": true, "venv": true, "__pycache__": true,
	".sb": true, "target": true, ".vercel": true, ".netlify": true,
}

// tarDir packs a directory into a gzip tarball, skipping dependency and build directories.
func tarDir(root string) ([]byte, int, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, 0, err
	}
	if !info.IsDir() {
		return nil, 0, fmt.Errorf("%s is not a directory", root)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Size() > 20<<20 {
			fmt.Fprintf(os.Stderr, "skipping large file %s (%d MB)\n", rel, fi.Size()>>20)
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		files++
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	if err := gz.Close(); err != nil {
		return nil, 0, err
	}
	if files == 0 {
		return nil, 0, fmt.Errorf("no files to upload in %s", root)
	}
	return buf.Bytes(), files, nil
}

// upload packs dir and uploads it, returning the upload id.
func (c *client) upload(dir string) (string, error) {
	data, files, err := tarDir(dir)
	if err != nil {
		return "", &exitErr{exitError, "error: " + err.Error()}
	}
	fmt.Fprintf(os.Stderr, "uploading %d files (%d KB) from %s\n", files, len(data)/1024, dir)
	req, err := http.NewRequest("POST", strings.TrimSuffix(c.cfg.API, "/")+"/v1/uploads", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", &exitErr{exitError, "upload failed: " + err.Error()}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var r struct {
		Upload string `json:"upload"`
		Error  string `json:"error"`
		Hint   string `json:"hint"`
	}
	json.Unmarshal(b, &r)
	if resp.StatusCode >= 300 || r.Upload == "" {
		msg := fmt.Sprintf("error: upload failed (HTTP %d): %s", resp.StatusCode, r.Error)
		if r.Hint != "" {
			msg += "\nhint: " + r.Hint
		}
		return "", &exitErr{exitError, msg}
	}
	return r.Upload, nil
}

// listUpload prints the files --dir would upload, without uploading.
func listUpload(root string) error {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if rel != "." && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, f := range files {
		fmt.Println(f)
	}
	data, _, err := tarDir(root)
	if err != nil {
		return err
	}
	sum, err := apps.ContentHash(bytes.NewReader(data))
	if err != nil {
		return err
	}
	fmt.Printf("%d files, %d KB compressed, content sha256 %s (skipped: %s)\n", len(files), len(data)/1024, sum, skipList())
	fmt.Println("note: compare with the content sha256 in `sb status <app>`; equal means the deployed source matches this folder (timestamps ignored).")
	return nil
}

func skipList() string {
	var out []string
	for k := range skipDirs {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}
