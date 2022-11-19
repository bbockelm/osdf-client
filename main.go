package stashcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	//"net/http"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"time"

	// "crypto/sha1"
	// "encoding/hex"
	// "strings"

	log "github.com/sirupsen/logrus"
)

type OptionsStruct struct {
	ProgressBars bool
	Recursive    bool
	Token        string
	Version      string
}

var Options OptionsStruct

var (
	version string
)

// Nearest cache
var NearestCache string

// List of caches, in order from closest to furthest
var NearestCacheList []string
var CachesJsonLocation string

// CacheOverride
var CacheOverride bool

type payloadStruct struct {
	filename     string
	sitename     string
	status       string
	Owner        string
	ProjectName  string
	version      string
	start1       int64
	end1         int64
	timestamp    int64
	downloadTime int64
	fileSize     int64
	downloadSize int64
}

/*
	Options from stashcache:
	--parser.add_option('-d', '--debug', dest='debug', action='store_true', help='debug')
	parser.add_option('-r', dest='recursive', action='store_true', help='recursively copy')
	parser.add_option('--closest', action='store_true', help="Return the closest cache and exit")
	--parser.add_option('-c', '--cache', dest='cache', help="Cache to use")
	parser.add_option('-j', '--caches-json', dest='caches_json', help="A JSON file containing the list of caches",
						default=None)
	parser.add_option('-n', '--cache-list-name', dest='cache_list_name', help="Name of pre-configured cache list to use",
						default=None)
	parser.add_option('--list-names', dest='list_names', action='store_true', help="Return the names of pre-configured cache lists and exit (first one is default for -n)")
	parser.add_option('--methods', dest='methods', help="Comma separated list of methods to try, in order.  Default: cvmfs,xrootd,http", default="cvmfs,xrootd,http")
	parser.add_option('-t', '--token', dest='token', help="Token file to use for reading and/or writing")
*/

// Determine the token name if it is embedded in the scheme, Condor-style
func getTokenName(destination *url.URL) (tokenName string) {
	schemePieces := strings.SplitN(destination.Scheme, "+", 2)
	tokenName = ""
	if len(schemePieces) > 1 {
		tokenName = schemePieces[1]
	}
	return
}

// Do writeback to stash using SciTokens
func doWriteBack(source string, destination *url.URL, namespace Namespace) (int64, error) {

	scitoken_contents, err := getToken(getTokenName(destination))
	if err != nil {
		return 0, err
	}
	return UploadFile(source, destination, scitoken_contents, namespace)

}

func getToken(token_name string) (string, error) {

	type tokenJson struct {
		AccessKey string `json:"access_token"`
		ExpiresIn int    `json:"expires_in"`
	}
	/*
		Search for the location of the authentiction token.  It can be set explicitly on the command line (TODO),
		with the environment variable "TOKEN", or it can be searched in the standard HTCondor directory pointed
		to by the environment variable "_CONDOR_CREDS".
	*/
	var token_location string
	if Options.Token != "" {
		token_location = Options.Token
		log.Debugln("Getting token location from command line:", Options.Token)
	} else {

		// WLCG Token Discovery
		if bearerToken, isBearerTokenSet := os.LookupEnv("BEARER_TOKEN"); isBearerTokenSet {
			return bearerToken, nil
		} else if bearerTokenFile, isBearerTokenFileSet := os.LookupEnv("BEARER_TOKEN_FILE"); isBearerTokenFileSet {
			if _, err := os.Stat(bearerTokenFile); err != nil {
				log.Warningln("Environment variable BEARER_TOKEN_FILE is set, but file being point to does not exist:", err)
			} else {
				token_location = bearerTokenFile
			}
		}
		if xdgRuntimeDir, xdgRuntimeDirSet := os.LookupEnv("XDG_RUNTIME_DIR"); token_location == "" && xdgRuntimeDirSet {
			// Get the uid
			uid := os.Getuid()
			tmpTokenPath := filepath.Join(xdgRuntimeDir, "bt_u"+strconv.Itoa(uid))
			if _, err := os.Stat(tmpTokenPath); err == nil {
				token_location = tmpTokenPath
			}
		}

		// Check for /tmp/bt_u<uid>
		if token_location == "" {
			uid := os.Getuid()
			tmpTokenPath := "/tmp/bt_u" + strconv.Itoa(uid)
			if _, err := os.Stat(tmpTokenPath); err == nil {
				token_location = tmpTokenPath
			}
		}

		// Backwards compatibility for getting scitokens
		// If TOKEN is not set in environment, and _CONDOR_CREDS is set, then...
		if tokenFile, isTokenSet := os.LookupEnv("TOKEN"); isTokenSet && token_location == "" {
			if _, err := os.Stat(tokenFile); err != nil {
				log.Warningln("Environment variable TOKEN is set, but file being point to does not exist:", err)
			} else {
				token_location = tokenFile
			}
		}

		// Finally, look in the HTCondor runtime
		token_filename := "scitokens.use"
		if len(token_name) > 0 {
			token_filename = token_name + ".use"
		}
		if credsDir, isCondorCredsSet := os.LookupEnv("_CONDOR_CREDS"); token_location == "" && isCondorCredsSet {
			// Token wasn't specified on the command line or environment, try the default scitoken
			if _, err := os.Stat(filepath.Join(credsDir, token_filename)); err != nil {
				log.Warningln("Environment variable _CONDOR_CREDS is set, but file being point to does not exist:", err)
			} else {
				token_location = filepath.Join(credsDir, token_filename)
			}
		}
		if _, err := os.Stat(".condor_creds/" + token_filename); err == nil && token_location == "" {
			token_location, _ = filepath.Abs(".condor_creds/" + token_filename)
		}
		if token_location == "" {
			// Print out, can't find token!  Print out error and exit with non-zero exit status
			// TODO: Better error message
			log.Errorln("Unable to find token file")
			return "", errors.New("failed to find token...")
		}
	}

	//Read in the JSON
	log.Debug("Opening token file: " + token_location)
	tokenContents, err := os.ReadFile(token_location)
	if err != nil {
		log.Errorln("Error reading token file:", err)
		return "", err
	}
	tokenParsed := tokenJson{}
	if err := json.Unmarshal(tokenContents, &tokenParsed); err != nil {
		log.Debugln("Error unmarshalling JSON token contents:", err)
		log.Debugln("Assuming the token file is not JSON, and only contains the TOKEN")
		tokenStr := strings.TrimSpace(string(tokenContents))
		return tokenStr, nil
	}
	return tokenParsed.AccessKey, nil
}

// Check the size of a remote file in an origin
func CheckOSDF(destination string, methods []string) (remoteSize uint64, err error) {

	defer func() {
		if r := recover(); r != nil {
			log.Errorln("Panic captured while attempting to perform size check:", r)
			ret := fmt.Sprintf("Unrecoverable error (panic) while check file size: %v", r)
			err = errors.New(ret)
			remoteSize = 0
		}
	}()

	dest_uri, err := url.Parse(destination)
	if err != nil {
		log.Errorln("Failed to parse destination URL")
		return 0, err
	}

	understoodSchemes := []string{"osdf", ""}

	_, foundSource := Find(understoodSchemes, dest_uri.Scheme)
	if !foundSource {
		log.Errorln("Unknown schema provided:", dest_uri.Scheme)
		return 0, errors.New("Unsupported scheme requested")
	}

	if dest_uri.Scheme == "" {
		dest_uri.Scheme = "osdf"
	}

	ns, err := MatchNamespace(dest_uri.Path)
	if err != nil {
		return 0, err
	}

	for _, method := range methods {

		switch method {
		case "http":
			log.Info("Trying HTTP...")
			if remoteSize, err = stat_http(dest_uri, ns); err == nil {
				return remoteSize, nil
			}
		default:
			log.Errorf("Unknown transfer method: %s", method)
			return 0, errors.New("Unknown transfer method")
		}
	}
	return 0, err
}


// Start the transfer, whether read or write back
func DoStashCPSingle(sourceFile string, destination string, methods []string, recursive bool) (bytesTransferred int64, err error) {

	// First, create a handler for any panics that occur
	defer func() {
		if r := recover(); r != nil {
			log.Errorln("Panic captured while attempting to perform transfer (DoStashCPSingle):", r)
			ret := fmt.Sprintf("Unrecoverable error (panic) captured in DoStashCPSingle: %v", r)
			err = errors.New(ret)
			bytesTransferred = 0

			// Attempt to add the panic to the error accumulator
			AddError(errors.New(ret))
		}
	}()

	// Parse the source and destination with URL parse

	source_url, err := url.Parse(sourceFile)
	if err != nil {
		log.Errorln("Failed to parse source URL:", err)
		return 0, err
	}

	dest_url, err := url.Parse(destination)
	if err != nil {
		log.Errorln("Failed to parse destination URL:", err)
		return 0, err
	}

	source_scheme_pieces := strings.SplitN(source_url.Scheme, "+", 2)
	dest_scheme_pieces := strings.SplitN(dest_url.Scheme, "+", 2)

	understoodSchemes := []string{"stash", "file", "osdf", ""}

	_, foundSource := Find(understoodSchemes, source_scheme_pieces[0])
	if !foundSource {
		log.Errorln("Do not understand source scheme:", source_url.Scheme)
		return 0, errors.New("Do not understand source scheme")
	}

	_, foundDest := Find(understoodSchemes, dest_scheme_pieces[0])
	if !foundDest {
		log.Errorln("Do not understand destination scheme:", source_url.Scheme)
		return 0, errors.New("Do not understand destination scheme")
	}

	// Get the namespace of the remote filesystem
	// For write back, it will be the destination
	// For read it will be the source.

	if dest_scheme_pieces[0] == "stash" || dest_scheme_pieces[0] == "osdf" {
		log.Debugln("Detected writeback")
		ns, err := MatchNamespace(dest_url.Path)
		if err != nil {
			log.Errorln("Failed to get namespace information:", err)
		}
		return doWriteBack(source_url.Path, dest_url, ns)
	}

	if dest_url.Scheme == "file" {
		destination = dest_url.Path
	}

	if source_scheme_pieces[0] == "stash" || source_scheme_pieces[0]== "osdf" {
		sourceFile = source_url.Path
	}

	if string(sourceFile[0]) != "/" {
		sourceFile = "/" + sourceFile
	}

	ns, err := MatchNamespace(source_url.Path)
	if err != nil {
		return 0, err
	}

	// get absolute path
	destPath, _ := filepath.Abs(destination)

	//Check if path exists or if its in a folder
	if destStat, err := os.Stat(destPath); os.IsNotExist(err) {
		destination = destPath
	} else if destStat.IsDir() {
		// Get the file name of the source
		sourceFilename := path.Base(sourceFile)
		destination = path.Join(destPath, sourceFilename)
	}

	payload := payloadStruct{}
	payload.version = version
	var found bool
	payload.sitename, found = os.LookupEnv("OSG_SITE_NAME")
	if !found {
		payload.sitename = "siteNotFound"
	}

	//Fill out the payload as much as possible
	payload.filename = source_url.Path

	// ??

	parse_job_ad(payload)

	payload.start1 = time.Now().Unix()

	// Go thru the download methods
	success := false

	// If recursive, only do http method to guarantee freshest directory contents
	if Options.Recursive {
		methods = []string{"http"}
	}

	token_name := getTokenName(source_url)

	// switch statement?
	var downloaded int64 = 0
Loop:
	for _, method := range methods {

		switch method {
		case "cvmfs":
			log.Info("Trying CVMFS...")
			if downloaded, err = download_cvmfs(sourceFile, destination, &payload); err == nil {
				success = true
				break Loop
				//check if break still works
			}
		case "xrootd":
			log.Info("Trying XROOTD...")
			if downloaded, err = download_xrootd(sourceFile, destination, &payload); err == nil {
				success = true
				break Loop
			}
		case "http":
			log.Info("Trying HTTP...")
			if downloaded, err = download_http(sourceFile, destination, &payload, ns, recursive, token_name); err == nil {
				success = true
				break Loop
			}
		default:
			log.Errorf("Unknown transfer method: %s", method)
		}
	}

	payload.end1 = time.Now().Unix()

	payload.timestamp = payload.end1
	payload.downloadTime = (payload.end1 - payload.start1)

	if success {
		payload.status = "Success"

		// Get the final size of the download file
		payload.fileSize = downloaded
		payload.downloadSize = downloaded
	} else {
		log.Error("All methods failed! Unable to download file.")
		payload.status = "Fail"
	}

	// We really don't care if the es send fails, but log
	// it in debug if it does fail
	if err := es_send(&payload); err != nil {
		log.Debugln("Failed to send to data to ES")
	}

	if !success {
		return downloaded, errors.New("failed to download file")
	} else {
		return downloaded, nil
	}

}

// Find takes a slice and looks for an element in it. If found it will
// return it's key, otherwise it will return -1 and a bool of false.
// From https://golangcode.com/check-if-element-exists-in-slice/
func Find(slice []string, val string) (int, bool) {
	for i, item := range slice {
		if item == val {
			return i, true
		}
	}
	return -1, false
}

// get_ips will resolve a hostname and return all corresponding IP addresses
// in DNS.  This can be used to randomly pick an IP when DNS round robin
// is used
func get_ips(name string) []string {
	var ipv4s []string
	var ipv6s []string

	info, err := net.LookupHost(name)
	if err != nil {
		log.Error("Unable to look up", name)

		var empty []string
		return empty
	}

	for _, addr := range info {
		parsedIP := net.ParseIP(addr)

		if parsedIP.To4() != nil {
			ipv4s = append(ipv4s, addr)
		} else if parsedIP.To16() != nil {
			ipv6s = append(ipv6s, "["+addr+"]")
		}
	}

	//Randomize the order of each
	rand.Seed(time.Now().UnixNano())
	rand.Shuffle(len(ipv4s), func(i, j int) { ipv4s[i], ipv4s[j] = ipv4s[j], ipv4s[i] })
	rand.Shuffle(len(ipv6s), func(i, j int) { ipv6s[i], ipv6s[j] = ipv6s[j], ipv6s[i] })

	// Always prefer IPv4
	return append(ipv4s, ipv6s...)

}

func parse_job_ad(payload payloadStruct) { // TODO: needs the payload

	//Parse the .job.ad file for the Owner (username) and ProjectName of the callee.

	condorJobAd, isPresent := os.LookupEnv("_CONDOR_JOB_AD")
	var filename string
	if isPresent {
		filename = condorJobAd
	} else if _, err := os.Stat(".job.ad"); err == nil {
		filename = ".job.ad"
	} else {
		return
	}

	// https://stackoverflow.com/questions/28574609/how-to-apply-regexp-to-content-in-file-go

	b, err := os.ReadFile(filename)
	if err != nil {
		log.Fatal(err)
	}

	// Get all matches from file
	classadRegex, e := regexp.Compile(`^\s*(Owner|ProjectName)\s=\s"(.*)"`)
	if e != nil {
		log.Fatal(e)
	}

	matches := classadRegex.FindAll(b, -1)

	for _, match := range matches {
		if string(match[0]) == "Owner" {
			payload.Owner = string(match[1])
		} else if string(match) == "ProjectName" {
			payload.ProjectName = string(match[1])
		}
	}

}

// NOT IMPLEMENTED
// func doStashcpdirectory(sourceDir string, destination string, methods string){

// 	// ?? sourceItems = to_str(subprocess.Popen(["xrdfs", stash_origin, "ls", sourceDir], stdout=subprocess.PIPE).communicate()[0]).split()

// 	// ?? for remote_file in sourceItems:

// 	command2 := "xrdfs " + stash_origin + " stat "+ remote_file + " | grep "IsDir" | wc -l"

// 	//	?? isdir=to_str(subprocess.Popen([command2],stdout=subprocess.PIPE,shell=True).communicate()[0].split()[0])isdir=to_str(subprocess.Popen([command2],stdout=subprocess.PIPE,shell=True).communicate()[0].split()[0])

// 	if isDir != 0 {
// 		result := doStashcpdirectory(remote, destination /*debug variable??*/)
// 	} else {
// 		result := doStashCpSingle(remote_file, destination, methods, debug)
// 	}

// 	// Stop the transfer if something fails
// 	if result != 0 {
// 		return result
// 	}

// 	return 0
// }

func es_send(payload *payloadStruct) error {

	// calculate the current timestamp
	timeStamp := time.Now().Unix()
	payload.timestamp = timeStamp

	// convert payload to a JSON string (something with Marshall ...)
	var jsonBytes []byte
	var err error
	if jsonBytes, err = json.Marshal(payload); err != nil {
		log.Errorln("Failed to marshal payload JSON: ", err)
		return err
	}

	errorChan := make(chan error)

	// Need to make a closure in order to handle the error
	go func() {
		err := doEsSend(jsonBytes, errorChan)
		if err != nil {
			return
		}
	}()

	select {
	case returnedError := <-errorChan:
		return returnedError
	case <-time.After(5 * time.Second):
		log.Debugln("Send to ES timed out")
		return errors.New("ES send timed out")
	}

}

// Do the actual send to ES
// Should be called with a timeout
func doEsSend(jsonBytes []byte, errorChannel chan<- error) error {
	// Send a HTTP POST to collector.atlas-ml.org, with a timeout!
	resp, err := http.Post("http://collector.atlas-ml.org:9951", "application/json", bytes.NewBuffer(jsonBytes))

	if err != nil {
		log.Errorln("Can't get collector.atlas-ml.org:", err)
		errorChannel <- err
		return err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Errorln("Failed to close body when uploading payload")
		}
	}(resp.Body)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		errorChannel <- err
		return err
	}
	log.Debugln("Returned from collector.atlas-ml.org:", string(body))
	errorChannel <- nil
	return nil
}
