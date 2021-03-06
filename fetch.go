package fetch

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/golang/glog"
	"github.com/open-horizon/anax/policy"
	"github.com/open-horizon/horizon-pkg-fetch/fetcherrors"
	"github.com/open-horizon/horizon-pkg-fetch/horizonpkg"
	"hash"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

func authenticatedRequest(pURL string, authCreds map[string]map[string]string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, pURL, nil)
	if err != nil {
		return nil, err
	}

	// matching them (for now) amounts to first prefix match wins
	for k, v := range authCreds {
		if strings.HasPrefix(pURL, k) {

			var username string
			if val, ok := v["username"]; ok {
				username = val
			}

			var password string
			if val, ok := v["password"]; ok {
				password = val
			}

			if username != "" && password != "" {
				glog.V(3).Infof("Using username %v in HTTPS Basic auth header to %v", username, pURL)
				req.SetBasicAuth(username, password)
				break
			}
		}
	}

	return req, nil
}

// side effect: stores the pkgMeta file in destinationDir
func fetchPkgMeta(client *http.Client, authCreds map[string]map[string]string, primarySigningKey string, userKeysDir string, pkgURL string, pkgURLSignature string, destinationDir string) (*horizonpkg.Pkg, error) {
	writeFile := func(destinationDir string, fileName string, content []byte) (string, error) {
		destFilePath := path.Join(destinationDir, fileName)
		// this'll overwrite
		if err := ioutil.WriteFile(destFilePath, content, 0600); err != nil {
			return "", fetcherrors.PkgMetaError{fmt.Sprintf("Failed to write file %v", destFilePath), err}
		}

		return destFilePath, nil
	}

	glog.V(5).Infof("Fetching Pkg from %v", pkgURL)

	req, err := authenticatedRequest(pkgURL, authCreds)
	if err != nil {
		return nil, err
	}

	// fetch, hydrate
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if response.StatusCode != http.StatusOK {
		return nil, fetcherrors.PkgMetaError{fmt.Sprintf("Unexpected status code in response to Horizon Pkg fetch: %v", response.StatusCode), fmt.Errorf("Failed to fetch Pkg meta from %v", pkgURL)}
	}
	defer response.Body.Close()
	rawBody, err := ioutil.ReadAll(response.Body)

	hasher := sha256.New()
	if _, err := io.Copy(hasher, bytes.NewReader(rawBody)); err != nil {
		return nil, fmt.Errorf("Unable to copy Pkg content into hash function. Error: %v", err)
	}

	if err := verifySignatureWithAnyKey(primarySigningKey, userKeysDir, hasher, []string{pkgURLSignature}); err != nil {

		return nil, fetcherrors.PkgMetaError{fmt.Sprintf("Pkg metadata failed cryptographic verification: %v", err), fmt.Errorf("Failure processing Pkg meta: %v and signature: %v", pkgURL, pkgURLSignature)}
	}

	var pkg horizonpkg.Pkg
	if err := json.Unmarshal(rawBody, &pkg); err != nil {
		return nil, err
	}

	fetchFilePath, err := writeFile(destinationDir, fmt.Sprintf("%v.json", pkg.ID), rawBody)
	if err != nil {
		return nil, err
	}

	glog.V(2).Infof("Wrote PkgMeta to %v", fetchFilePath)

	// TODO: dump all pkg content (both meta and parts) to debug

	return &pkg, nil
}

func precheckPkgParts(pkg *horizonpkg.Pkg) error {
	for _, part := range pkg.Parts {
		repoTag, exists := pkg.Meta.Provides.Images[part.ID]
		if !exists {
			return fmt.Errorf("Error in pkg file: Meta.Provides is expected to contain metadata about each part and it is missing info about part %v", part)
		}
		glog.V(2).Infof("Precheck of container %v (Pkg part id: %v) passed, will fetch it", repoTag, part.ID)

	}

	return nil
}

// VerificationError extends error, indicating a problem verifying a Pkg part
type VerificationError struct {
	msg string
}

// Error returns the error message in this error
func (e VerificationError) Error() string {
	return e.msg
}

type fetchErrRecorder struct {
	Errors    map[string]error
	WriteLock *sync.Mutex
}

func (r fetchErrRecorder) String() string {
	return fmt.Sprintf("Errors: %v", r.Errors)
}

func newFetchErrRecorder() fetchErrRecorder {
	return fetchErrRecorder{
		Errors:    make(map[string]error),
		WriteLock: &sync.Mutex{},
	}
}

type partFetchFailure struct {
	HTTPStatusCode int
	PartURL        string
}

func fetchPkgPart(client *http.Client, authCreds map[string]map[string]string, pkgURLBase string, partPath string, expectedBytes int64, sources []horizonpkg.PartSource) error {
	tryOpen := func(path string) (*os.File, error) {
		return os.OpenFile(partPath, os.O_RDWR|os.O_CREATE, 0600)
	}

	tryRemove := func(f *os.File, msg string) error {
		glog.Error(msg)

		f.Close()
		err := os.Remove(f.Name())
		if err != nil {
			return err
		}

		return nil
	}

	var partFile *os.File
	var openErr error
	partFile, openErr = tryOpen(partPath)

	if openErr != nil && os.IsExist(openErr) {

		info, statErr := os.Stat(partPath)
		if statErr != nil {
			err := tryRemove(partFile, fmt.Sprintf("Error getting status for file %v although it exists. Will attempt to delete it and continue", partPath))
			if err != nil {
				return err
			}

		} else if info.Size() == expectedBytes {
			glog.V(3).Infof("Part file %v exists on disk and it has the appropriate size, skipping redownload", partPath)
			return nil
		} else {
			// TODO: can try resume here if we have an HTTP server that knows how to handle it
			err := tryRemove(partFile, fmt.Sprintf("Part file %v exists on disk but it's not complete (%v bytes and should be %v bytes). Deleting it and trying again", partPath, info.Size(), expectedBytes))
			if err != nil {
				return err
			}
		}
		partFile.Close()
		partFile, openErr = tryOpen(partPath)
		if openErr != nil {
			return openErr
		}
	}

	var fetchFailure *partFetchFailure

	// we are clean, try download
	for _, source := range sources {
		var pURL string
		if strings.HasPrefix(source.URL, "/") {
			// it's an absolute path but we need to prepend the Pkg's domain, it's assumed by convention
			pURL = fmt.Sprintf("%s%s", pkgURLBase, source.URL)
			glog.V(3).Infof("Part has absolute URL path but assumes domain by convention. Composed full URL %v using domain from Pkg URL", pURL)
		} else {
			pURL = source.URL
		}

		fetchFailure = nil

		req, err := authenticatedRequest(pURL, authCreds)
		if err != nil {
			return err
		}

		// fetch, hydrate
		response, err := client.Do(req)
		if err != nil || response.StatusCode != http.StatusOK {
			glog.Errorf("Failed to download part %v from %v (using url %v). Response: %v. Error: %v", partPath, source, pURL, response, err)
			fetchFailure = &partFetchFailure{response.StatusCode, pURL}
		} else {
			defer response.Body.Close()
			bytes, err := io.Copy(partFile, response.Body)
			if err != nil {
				return fmt.Errorf("IO copy from HTTP response body failed on part: %v. Error: %v", partPath, err)
			}

			if bytes != expectedBytes {
				glog.Errorf("Error in download and copy of part %v from %v (using url %v)", partPath, source, pURL)

				// ignore error, give it another shot
				tryRemove(partFile, fmt.Sprintf("Error in download and copy of part %v from %v (using url %v)", partPath, source, pURL))

				partFile, openErr = tryOpen(partPath)
				if openErr != nil {
					return openErr
				}
				defer partFile.Close()
				continue
			} else {
				glog.V(2).Infof("Successfully wrote %v", partPath)
				return nil
			}
		}
	}

	internalError := fmt.Errorf("Part could not be fetched: %v from any of its sources: %v", partPath, sources)

	// if this isn't nil, we failed on at least the most recent source and report it
	if fetchFailure != nil {
		if fetchFailure.HTTPStatusCode == 401 || fetchFailure.HTTPStatusCode == 403 {
			return fetcherrors.PkgSourceFetchAuthError{fmt.Sprintf("Authentication or Authorization error attempting to fetch part from URL: %v. HTTP Status code: %v", fetchFailure.PartURL, fetchFailure.HTTPStatusCode), internalError}
		}

		return fetcherrors.PkgSourceFetchError{fmt.Sprintf("Error when fetching part from URL: %v. HTTP Status code: %v", fetchFailure.PartURL, fetchFailure.HTTPStatusCode), internalError}
	}

	// try fetching a part from each source, if all fail exit with error
	return fetcherrors.PkgSourceFetchError{fmt.Sprintf("Failed to complete fetch."), internalError}
}

// all provided signatures must match keys in userKeysDir
func verifyPkgPart(primarySigningKey string, userKeysDir string, partPath string, partHash string, signatures []string) error {

	glog.V(5).Infof("Verifying pkg part %v with userKeysDir %v and signatures %v", partPath, userKeysDir, signatures)

	partFile, err := os.Open(partPath)
	if err != nil {
		return err
	}
	defer partFile.Close()

	// Read the file content into the hash function.
	hasher := sha256.New()
	if _, err := io.Copy(hasher, partFile); err != nil {
		return fmt.Errorf("Unable to copy image file content into hash function for part %v. Error: %v", partPath, err)
	}

	// check the hash first
	actualHash := fmt.Sprintf("%x", string(hasher.Sum(nil)))
	if partHash != actualHash {
		// delete file too
		partFile.Close()
		err := os.Remove(partPath)
		if err != nil {
			glog.Errorf("Failed to remove part %v after failed hash check. Error: %v", partPath, err)
		}
		return fetcherrors.PkgSignatureVerificationError{fmt.Sprintf("Mismatch between expected hash, %v and actual hash.", partHash, actualHash), fmt.Errorf("Part failed verification: %v", partPath)}
	}

	if err := verifySignatureWithAnyKey(primarySigningKey, userKeysDir, hasher, signatures); err == nil {
		// verified
		return nil
	}

	return fetcherrors.PkgSignatureVerificationError{fmt.Sprintf("Part failed cryptographic verification: %v", err), fmt.Errorf("Part failed verification: %v", partPath)}
}

func verifySignatureWithAnyKey(primarySigningKey string, userKeysDir string, hasher hash.Hash, signatures []string) error {

	// this is computationally expensive
	for _, sig := range signatures {
		// TODO: refactor this code, extract verification into rsapss-tool; for efficiency, perhaps we should give keys IDs and include those in the pkg signature
		glog.V(7).Infof("Verifying with sig: %v, userKeysDir: %v", sig, userKeysDir)
		verified, err := policy.VerifyWorkload(primarySigningKey, sig, hasher, userKeysDir)
		if err != nil {
			return err
		}

		if verified {
			return nil
		}
	}

	return VerificationError{}
}

func fetchAndVerify(httpClientFactory func(overrideTimeoutS *uint) *http.Client, authCreds map[string]map[string]string, pkgURLBase string, parts horizonpkg.DockerImageParts, destinationDir string, primarySigningKey string, userKeysDir string) ([]string, error) {
	fetchErrs := newFetchErrRecorder()
	var fetched []string

	addResult := func(id string, err error, partPath string) {
		fetchErrs.WriteLock.Lock()
		defer fetchErrs.WriteLock.Unlock()

		if err != nil {
			// record failures

			glog.V(6).Infof("Recording fetch error: %v with key: %v", err, id)
			fetchErrs.Errors[id] = err
		} else if partPath != "" {
			// success

			var abs string
			abs, err = filepath.Abs(partPath)
			if err != nil {
				fetchErrs.Errors[id] = err
			} else {
				fetched = append(fetched, abs)
			}
		}
	}

	var group sync.WaitGroup

	for name, part := range parts {

		group.Add(1)

		// wrap up the functionality per part; (note that we avoid problematic closed-over iteration vars in the go routine)
		go func(name string, part horizonpkg.DockerImagePart) {
			defer group.Done()

			// we don't care about file extensions if they're not in the ID
			partPath := path.Join(destinationDir, name)

			glog.V(5).Infof("Dispatched goroutine to download (%v) to path: %v (part: %v)", name, partPath, part)

			var timeoutS uint
			if part.Bytes <= 1024*1024 {
				timeoutS = uint(120)
			} else {
				timeoutS = uint((part.Bytes * 8) / 1024 / 100)
			}

			glog.V(2).Infof("Fetching %v", part.ID)
			addResult(name, fetchPkgPart(httpClientFactory(&timeoutS), authCreds, pkgURLBase, partPath, part.Bytes, part.Sources), "")

			// TODO: support retries here
			if len(fetchErrs.Errors) == 0 {
				glog.V(2).Infof("Verifying %v", part)
				addResult(name, verifyPkgPart(primarySigningKey, userKeysDir, partPath, part.Sha256sum, part.Signatures), partPath)
			}

		}(name, part)
	}

	group.Wait()

	if len(fetchErrs.Errors) > 0 {
		return nil, fmt.Errorf("Error fetching parts. Errors: %v", &fetchErrs)
	}

	return fetched, nil
}

// PkgFetch fetches a pkg metadata file from the given URL and then verifies
// the content of the pkg.
//     pkgURL is the URL of the pkg file containing the image content
func PkgFetch(httpClientFactory func(overrideTimeoutS *uint) *http.Client, pkgURL url.URL, pkgURLSignature string, destinationDir string, primarySigningKey string, userKeysDir string, authCreds map[string]map[string]string) ([]string, error) {
	mkdirs := func(pp string) error {
		if err := os.MkdirAll(pp, 0700); err != nil {
			return err
		}
		return nil
	}

	client := httpClientFactory(nil)

	if pkgURLSignature == "" {
		return nil, fmt.Errorf("Disabling Pkg file signature checking not supported")
	}

	// make pkg subdirectory in destination directory
	if err := mkdirs(destinationDir); err != nil {
		return nil, fetcherrors.PkgSourceError{"Failed creating Pkg destination dirs on host", err}
	}

	pkg, err := fetchPkgMeta(client, authCreds, primarySigningKey, userKeysDir, pkgURL.String(), pkgURLSignature, destinationDir)
	if err != nil {
		return nil, err
	}

	// we do this separately so we have a greater chance of the async fetches succeeding before we start them all
	if err := precheckPkgParts(pkg); err != nil {
		return nil, fetcherrors.PkgPrecheckError{"Failed to validate Pkg information before fetching", err}
	}

	pkgDestinationDir := path.Join(destinationDir, pkg.ID)
	if err := mkdirs(pkgDestinationDir); err != nil {
		return nil, fetcherrors.PkgSourceError{"Failed creating Pkg destination dirs on host", err}
	}

	pkgURLParts := strings.Split(pkgURL.String(), "/")
	pkgURLBase := strings.Join(pkgURLParts[0:len(pkgURLParts)-1], "/")

	glog.V(4).Infof("Extracted pkgURLBase %v from pkgURL %v", pkgURLBase, pkgURL.String())

	var fetched []string
	fetched, err = fetchAndVerify(httpClientFactory, authCreds, pkgURLBase, pkg.Parts, pkgDestinationDir, primarySigningKey, userKeysDir)
	if err != nil {
		return nil, err
	}

	// TODO: expand to return the .fetch file; also shortcut some fetch operations if it exists
	// for now we just return the old-style image files slice

	return fetched, nil
}
