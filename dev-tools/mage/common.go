// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package mage

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
	"unicode"

	"github.com/magefile/mage/sh"
	"github.com/magefile/mage/target"
	"github.com/magefile/mage/types"
	"github.com/pkg/errors"
)

// Expand expands the given Go text/template string.
func Expand(in string, args ...map[string]interface{}) (string, error) {
	return expandTemplate("inline", in, FuncMap, EnvMap(args...))
}

// MustExpand expands the given Go text/template string. It panics if there is
// an error.
func MustExpand(in string, args ...map[string]interface{}) string {
	out, err := Expand(in, args...)
	if err != nil {
		panic(err)
	}
	return out
}

// ExpandFile expands the Go text/template read from src and writes the output
// to dst.
func ExpandFile(src, dst string, args ...map[string]interface{}) error {
	return expandFile(src, dst, EnvMap(args...))
}

// MustExpandFile expands the Go text/template read from src and writes the
// output to dst. It panics if there is an error.
func MustExpandFile(src, dst string, args ...map[string]interface{}) {
	if err := ExpandFile(src, dst, args...); err != nil {
		panic(err)
	}
}

func expandTemplate(name, tmpl string, funcs template.FuncMap, args ...map[string]interface{}) (string, error) {
	t := template.New(name).Option("missingkey=error")
	if len(funcs) > 0 {
		t = t.Funcs(funcs)
	}

	t, err := t.Parse(tmpl)
	if err != nil {
		if name == "inline" {
			return "", errors.Wrapf(err, "failed to parse template '%v'", tmpl)
		}
		return "", errors.Wrap(err, "failed to parse template")
	}

	buf := new(bytes.Buffer)
	if err := t.Execute(buf, joinMaps(args...)); err != nil {
		if name == "inline" {
			return "", errors.Wrapf(err, "failed to expand template '%v'", tmpl)
		}
		return "", errors.Wrap(err, "failed to expand template")
	}

	return buf.String(), nil
}

func joinMaps(args ...map[string]interface{}) map[string]interface{} {
	switch len(args) {
	case 0:
		return nil
	case 1:
		return args[0]
	}

	var out map[string]interface{}
	for _, m := range args {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func expandFile(src, dst string, args ...map[string]interface{}) error {
	tmplData, err := ioutil.ReadFile(src)
	if err != nil {
		return errors.Wrapf(err, "failed reading from template %v", src)
	}

	output, err := expandTemplate(src, string(tmplData), FuncMap, args...)
	if err != nil {
		return err
	}

	dst, err = expandTemplate("inline", dst, FuncMap, args...)
	if err != nil {
		return err
	}

	if err = ioutil.WriteFile(createDir(dst), []byte(output), 0644); err != nil {
		return errors.Wrap(err, "failed to write rendered template")
	}

	return nil
}

// CWD return the current working directory.
func CWD() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(errors.Wrap(err, "failed to get the CWD"))
	}
	return wd
}

// EnvOr returns the value of the specified environment variable if it is
// non-empty. Otherwise it return def.
func EnvOr(name, def string) string {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	return s
}

var (
	dockerInfoValue *DockerInfo
	dockerInfoErr   error
	dockerInfoOnce  sync.Once
)

// DockerInfo contains information about the docker daemon.
type DockerInfo struct {
	OperatingSystem string   `json:"OperatingSystem"`
	Labels          []string `json:"Labels"`
	NCPU            int      `json:"NCPU"`
	MemTotal        int      `json:"MemTotal"`
}

// IsBoot2Docker returns true if the Docker OS is boot2docker.
func (info *DockerInfo) IsBoot2Docker() bool {
	return strings.Contains(strings.ToLower(info.OperatingSystem), "boot2docker")
}

// HaveDocker returns an error if docker is unavailable.
func HaveDocker() error {
	if _, err := GetDockerInfo(); err != nil {
		return errors.Wrap(err, "docker is not available")
	}
	return nil
}

// GetDockerInfo returns data from the docker info command.
func GetDockerInfo() (*DockerInfo, error) {
	dockerInfoOnce.Do(func() {
		dockerInfoValue, dockerInfoErr = dockerInfo()
	})

	return dockerInfoValue, dockerInfoErr
}

func dockerInfo() (*DockerInfo, error) {
	data, err := sh.Output("docker", "info", "-f", "{{ json .}}")
	if err != nil {
		return nil, err
	}

	var info DockerInfo
	if err = json.Unmarshal([]byte(data), &info); err != nil {
		return nil, err
	}

	return &info, nil
}

// FindReplace reads a file, performs a find/replace operation, then writes the
// output to the same file path.
func FindReplace(file string, re *regexp.Regexp, repl string) error {
	info, err := os.Stat(file)
	if err != nil {
		return err
	}

	contents, err := ioutil.ReadFile(file)
	if err != nil {
		return err
	}

	out := re.ReplaceAllString(string(contents), repl)
	return ioutil.WriteFile(file, []byte(out), info.Mode().Perm())
}

// MustFindReplace invokes FindReplace and panics if an error occurs.
func MustFindReplace(file string, re *regexp.Regexp, repl string) {
	if err := FindReplace(file, re, repl); err != nil {
		panic(errors.Wrap(err, "failed to find and replace"))
	}
}

// Copy copies a file or a directory (recursively) and preserves the permissions.
func Copy(src, dest string) error {
	info, err := os.Stat(src)
	if err != nil {
		return errors.Wrapf(err, "failed to stat source file %v", src)
	}
	return recursiveCopy(src, dest, info)
}

func fileCopy(src, dest string, info os.FileInfo) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	if !info.Mode().IsRegular() {
		return errors.Errorf("failed to copy source file because it is not a regular file")
	}

	destFile, err := os.OpenFile(createDir(dest), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode()&os.ModePerm)
	if err != nil {
		return err
	}
	defer destFile.Close()

	if _, err = io.Copy(destFile, srcFile); err != nil {
		return err
	}
	return destFile.Close()
}

func dirCopy(src, dest string, info os.FileInfo) error {
	if err := os.MkdirAll(dest, info.Mode()); err != nil {
		return errors.Wrap(err, "failed creating dirs")
	}

	contents, err := ioutil.ReadDir(src)
	if err != nil {
		return errors.Wrapf(err, "failed to read dir %v", src)
	}

	for _, info := range contents {
		srcFile := filepath.Join(src, info.Name())
		destFile := filepath.Join(dest, info.Name())
		if err = recursiveCopy(srcFile, destFile, info); err != nil {
			return errors.Wrapf(err, "failed to copy %v to %v", srcFile, destFile)
		}
	}

	return nil
}

func recursiveCopy(src, dest string, info os.FileInfo) error {
	if info.IsDir() {
		return dirCopy(src, dest, info)
	}
	return fileCopy(src, dest, info)
}

// DownloadFile downloads the given URL and writes the file to destinationDir.
// The path to the file is returned.
func DownloadFile(url, destinationDir string) (string, error) {
	log.Println("Downloading", url)

	resp, err := http.Get(url)
	if err != nil {
		return "", errors.Wrap(err, "http get failed")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", errors.Errorf("download failed with http status: %v", resp.StatusCode)
	}

	name := filepath.Join(destinationDir, filepath.Base(url))
	f, err := os.Create(createDir(name))
	if err != nil {
		return "", errors.Wrap(err, "failed to create output file")
	}
	defer f.Close()

	if _, err = io.Copy(f, resp.Body); err != nil {
		return "", errors.Wrap(err, "failed to write file")
	}

	return name, f.Close()
}

// Extract extracts .zip, .tar.gz, or .tgz files to destinationDir.
func Extract(sourceFile, destinationDir string) error {
	ext := filepath.Ext(sourceFile)
	switch {
	case strings.HasSuffix(sourceFile, ".tar.gz"), ext == ".tgz":
		return untar(sourceFile, destinationDir)
	case ext == ".zip":
		return unzip(sourceFile, destinationDir)
	default:
		return errors.Errorf("failed to extract %v, unhandled file extension", sourceFile)
	}
}

func unzip(sourceFile, destinationDir string) error {
	r, err := zip.OpenReader(sourceFile)
	if err != nil {
		return err
	}
	defer r.Close()

	if err = os.MkdirAll(destinationDir, 0755); err != nil {
		return err
	}

	extractAndWriteFile := func(f *zip.File) error {
		innerFile, err := f.Open()
		if err != nil {
			return err
		}
		defer innerFile.Close()

		path := filepath.Join(destinationDir, f.Name)
		if !strings.HasPrefix(path, destinationDir) {
			return errors.Errorf("illegal file path in zip: %v", f.Name)
		}

		if f.FileInfo().IsDir() {
			return os.MkdirAll(path, f.Mode())
		}

		if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}

		out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}
		defer out.Close()

		if _, err = io.Copy(out, innerFile); err != nil {
			return err
		}

		return out.Close()
	}

	for _, f := range r.File {
		err := extractAndWriteFile(f)
		if err != nil {
			return err
		}
	}

	return nil
}

func untar(sourceFile, destinationDir string) error {
	file, err := os.Open(sourceFile)
	if err != nil {
		return err
	}
	defer file.Close()

	var fileReader io.ReadCloser = file

	if strings.HasSuffix(sourceFile, ".gz") {
		if fileReader, err = gzip.NewReader(file); err != nil {
			return err
		}
		defer fileReader.Close()
	}

	tarReader := tar.NewReader(fileReader)

	for {
		header, err := tarReader.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		path := filepath.Join(destinationDir, header.Name)
		if !strings.HasPrefix(path, destinationDir) {
			return errors.Errorf("illegal file path in tar: %v", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(path, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			writer, err := os.Create(path)
			if err != nil {
				return err
			}

			if _, err = io.Copy(writer, tarReader); err != nil {
				return err
			}

			if err = os.Chmod(path, os.FileMode(header.Mode)); err != nil {
				return err
			}

			if err = writer.Close(); err != nil {
				return err
			}
		default:
			return errors.Errorf("unable to untar type=%c in file=%s", header.Typeflag, path)
		}
	}

	return nil
}

func isSeparator(r rune) bool {
	return unicode.IsSpace(r) || r == ',' || r == ';'
}

// RunCmds runs the given commands and stops upon the first error.
func RunCmds(cmds ...[]string) error {
	for _, cmd := range cmds {
		if err := sh.Run(cmd[0], cmd[1:]...); err != nil {
			return err
		}
	}
	return nil
}

var (
	parallelJobsLock      sync.Mutex
	parallelJobsSemaphore chan int
)

func parallelJobs() chan int {
	parallelJobsLock.Lock()
	defer parallelJobsLock.Unlock()

	if parallelJobsSemaphore == nil {
		max := numParallel()
		parallelJobsSemaphore = make(chan int, max)
		log.Println("Max parallel jobs =", max)
	}

	return parallelJobsSemaphore
}

func numParallel() int {
	if maxParallel := os.Getenv("MAX_PARALLEL"); maxParallel != "" {
		if num, err := strconv.Atoi(maxParallel); err == nil && num > 0 {
			return num
		}
	}

	// To be conservative use the minimum of the number of CPUs between the host
	// and the Docker host.
	maxParallel := runtime.NumCPU()

	info, err := GetDockerInfo()
	if err == nil && info.NCPU < maxParallel {
		maxParallel = info.NCPU
	}

	return maxParallel
}

// ParallelCtx runs the given functions in parallel with an upper limit set
// based on GOMAXPROCS. The provided ctx is passed to the functions (if they
// accept it as a param).
func ParallelCtx(ctx context.Context, fns ...interface{}) {
	var fnWrappers []func(context.Context) error
	for _, f := range fns {
		fnWrapper := types.FuncTypeWrap(f)
		if fnWrapper == nil {
			panic("attempted to add a dep that did not match required function type")
		}
		fnWrappers = append(fnWrappers, fnWrapper)
	}

	var mu sync.Mutex
	var errs []string
	var wg sync.WaitGroup

	for _, fw := range fnWrappers {
		wg.Add(1)
		go func(fw func(context.Context) error) {
			defer func() {
				if v := recover(); v != nil {
					mu.Lock()
					errs = append(errs, fmt.Sprint(v))
					mu.Unlock()
				}
				wg.Done()
				<-parallelJobs()
			}()
			waitStart := time.Now()
			parallelJobs() <- 1
			log.Println("Parallel job waited", time.Since(waitStart), "before starting.")
			if err := fw(ctx); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprint(err))
				mu.Unlock()
			}
		}(fw)
	}

	wg.Wait()
	if len(errs) > 0 {
		panic(errors.Errorf(strings.Join(errs, "\n")))
	}
}

// Parallel runs the given functions in parallel with an upper limit set based
// on GOMAXPROCS.
func Parallel(fns ...interface{}) {
	ParallelCtx(context.Background(), fns...)
}

// FindFiles return a list of file matching the given glob patterns.
func FindFiles(globs ...string) ([]string, error) {
	var configFiles []string
	for _, glob := range globs {
		files, err := filepath.Glob(glob)
		if err != nil {
			return nil, errors.Wrapf(err, "failed on glob %v", glob)
		}
		configFiles = append(configFiles, files...)
	}
	return configFiles, nil
}

// FileConcat concatenates files and writes the output to out.
func FileConcat(out string, perm os.FileMode, files ...string) error {
	f, err := os.OpenFile(createDir(out), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return errors.Wrap(err, "failed to create file")
	}
	defer f.Close()

	w := bufio.NewWriter(f)

	append := func(file string) error {
		in, err := os.Open(file)
		if err != nil {
			return err
		}
		defer in.Close()

		if _, err := io.Copy(w, in); err != nil {
			return err
		}

		return nil
	}

	for _, in := range files {
		if err := append(in); err != nil {
			return err
		}
	}

	if err = w.Flush(); err != nil {
		return err
	}
	return f.Close()
}

// MustFileConcat invokes FileConcat and panics if an error occurs.
func MustFileConcat(out string, perm os.FileMode, files ...string) {
	if err := FileConcat(out, perm, files...); err != nil {
		panic(err)
	}
}

// VerifySHA256 reads a file and verifies that its SHA256 sum matches the
// specified hash.
func VerifySHA256(file string, hash string) error {
	f, err := os.Open(file)
	if err != nil {
		return errors.Wrap(err, "failed to open file for sha256 verification")
	}
	defer f.Close()

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return errors.Wrap(err, "failed reading from input file")
	}

	computedHash := hex.EncodeToString(sum.Sum(nil))
	expectedHash := strings.TrimSpace(hash)

	if computedHash != expectedHash {
		return errors.Errorf("SHA256 verification of %v failed. Expected=%v, "+
			"but computed=%v", f.Name(), expectedHash, computedHash)
	}
	log.Println("SHA256 OK:", f.Name())

	return nil
}

// CreateSHA512File computes the sha512 sum of the specified file the writes
// a sidecar file containing the hash and filename.
func CreateSHA512File(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return errors.Wrap(err, "failed to open file for sha512 summing")
	}
	defer f.Close()

	sum := sha512.New()
	if _, err := io.Copy(sum, f); err != nil {
		return errors.Wrap(err, "failed reading from input file")
	}

	computedHash := hex.EncodeToString(sum.Sum(nil))
	out := fmt.Sprintf("%v  %v", computedHash, filepath.Base(file))

	return ioutil.WriteFile(file+".sha512", []byte(out), 0644)
}

// IsUpToDate returns true iff dst exists and is older based on modtime than all
// of the sources.
func IsUpToDate(dst string, sources ...string) bool {
	if len(sources) == 0 {
		panic("No sources passed to IsUpToDate")
	}
	execute, err := target.Path(dst, sources...)
	return err == nil && !execute
}

// createDir creates the parent directory for the given file.
func createDir(file string) string {
	// Create the output directory.
	if dir := filepath.Dir(file); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			panic(errors.Wrapf(err, "failed to create parent dir for %v", file))
		}
	}
	return file
}

// binaryExtension returns the appropriate file extension based on GOOS.
func binaryExtension(goos string) string {
	if goos == "windows" {
		return ".exe"
	}
	return ""
}
