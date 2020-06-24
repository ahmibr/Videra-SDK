package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"time"
	"flag"
	"github.com/hashicorp/go-retryablehttp"
)

var chunkSize = int64(4 << 20) // 4 MB

var masterURL string
var uploadURL string
var id string

const defaultMaxRetries = 3
const defaultWaitingTime = 10

var mastersURLS []string
var lastUsedMaster = -1

var modelUploadOrder = []string{"model", "config", "code"}

// newClient is a function that returns customized http client
func newClient(maxRetries int, waitingTime int) *http.Client {
	clientretry := retryablehttp.NewClient()
	clientretry.RetryMax = maxRetries
	clientretry.RetryWaitMin = time.Duration(time.Duration(waitingTime) * time.Second)
	clientretry.RetryWaitMax = time.Duration(time.Duration(waitingTime) * time.Second)

	return clientretry.StandardClient()
}

// updateMasterURL is a function responsible for updating master node ip for future requests
func updateMasterURL() {
	// Should do master discovery by looping on nodes IPs in configuration file
	// and when a node responds with the master IP, it'll be set in master URL
	lastUsedMaster = (lastUsedMaster + 1) % len(mastersURLS)
	masterURL = mastersURLS[lastUsedMaster]
}

// updateUploadURL is a function responsible for asking master node for data node upload url
func updateUploadURL() error {
	// send request to master node to get data node upload ip
	// if success, set the new upload URL
	// if fail, return error
	client := newClient(defaultMaxRetries, defaultWaitingTime)
	req, _ := http.NewRequest(http.MethodGet, masterURL, nil)
	res, err := client.Do(req)
	if err != nil {
		log.Println(err)
		return err
	}

	defer res.Body.Close()

	bodyBytes, err := ioutil.ReadAll(res.Body)
	if err != nil {
		log.Fatal(err)
	}

	body := string(bodyBytes)

	if res.StatusCode != http.StatusOK {
		log.Println(body)
		return errors.New(body)
	}

	uploadURL = body
	log.Println(fmt.Sprintf("Updated upload url to %s", uploadURL))
	return nil
}

// getFileSize is a function to get file size
func getFileSize(filepath string) (int64, error) {
	fi, err := os.Stat(filepath)
	if err != nil {
		return 0, err
	}

	return fi.Size(), nil
}

// sendInitialRequest is a function responsible for starting upload process with data node
// default set headers are filename and filetype
func sendInitialRequest(filepath string, filetype string, extraHeaders map[string]string) (string, error) {
	filename := path.Base(filepath)

	client := newClient(defaultMaxRetries, defaultWaitingTime)
	req, _ := http.NewRequest(http.MethodPost, uploadURL, nil)
	req.Header.Set("Request-Type", "init")
	req.Header.Set("Filename", filename)
	req.Header.Set("Filetype", filetype)

	for key, val := range extraHeaders {
		req.Header.Set(key, val)
	}
	res, err := client.Do(req)
	if err != nil {
		log.Println(err)
		return "", err
	}

	if res.StatusCode != http.StatusCreated {
		log.Fatal(res.StatusCode)
	}

	id := res.Header.Get("ID")
	if res.Header.Get("Max-Request-Size") != "" {
		chunkSize, _ := strconv.ParseInt(res.Header.Get("Max-Request-Size"), 10, 64)
		log.Println(fmt.Sprintf("Chunk size %v", chunkSize))
	}
	return id, nil
}

// sendVideoInitialRequest is a function responsible for sending initial upload request for video
func sendVideoInitialRequest(videoPath string) (string, error) {
	videoSize, err := getFileSize(videoPath)
	if err != nil {
		log.Fatal(err)
	}

	headers := map[string]string{
		"Filesize": fmt.Sprintf("%v", videoSize),
	}
	return sendInitialRequest(videoPath, "video", headers)
}

// sendModelInitialRequest is a function responsible for sending initial upload request for model
func sendModelInitialRequest(modelPath string, configPath string, codePath string) (string, error) {
	modelSize, err := getFileSize(modelPath)
	if err != nil {
		log.Fatal(err)
	}
	configSize, err := getFileSize(configPath)
	if err != nil {
		log.Fatal(err)
	}
	codeSize, err := getFileSize(codePath)
	if err != nil {
		log.Fatal(err)
	}

	headers := map[string]string{
		"Filesize":    fmt.Sprintf("%v", modelSize+configSize+codeSize),
		"Model-Size":  fmt.Sprintf("%v", modelSize),
		"Config-Size": fmt.Sprintf("%v", configSize),
		"Code-Size":   fmt.Sprintf("%v", codeSize),
	}

	return sendInitialRequest(modelPath, "model", headers)
}

// uploadFiles is a function responsible for uploading files contents to data node
func uploadFiles(id string, filesPaths map[string]string, uploadOrder []string) error {
	client := newClient(defaultMaxRetries, defaultWaitingTime)

	buffer := make([]byte, chunkSize)
	offset := int64(0)

	for idx, fileName := range uploadOrder {
		file, err := os.Open(filesPaths[fileName])
		log.Println("Uploading", fileName, file.Name())
		defer file.Close()
		if err != nil {
			fmt.Println(err)
			return err
		}

		for {
			bytesread, err := file.Read(buffer)

			if err != nil {
				if err == io.EOF {
					if idx == len(modelUploadOrder)-1 {
						// reached the end of last file, but didn't receive ack from server
						return err
					}
					// finished current file
					file.Close()
					break
				}
				return err
			}

			r := bytes.NewReader(buffer[:bytesread])

			req, _ := http.NewRequest(http.MethodPost, uploadURL, r)
			req.Header.Set("Request-Type", "APPEND")
			req.Header.Set("ID", id)
			req.Header.Set("Offset", strconv.FormatInt(offset, 10))

			res, err := client.Do(req)
			if err != nil {
				log.Println(err)
				return err
			}
			if res.StatusCode != http.StatusOK {
				if res.StatusCode == http.StatusCreated {
					file.Close()
					return nil
				} else if res.Header.Get("Offset") != "" {
					newOffset, _ := strconv.ParseInt(res.Header.Get("Offset"), 10, 64)
					log.Println(fmt.Sprintf("Offset error: changing from %v to %v", offset, newOffset))
					offset = newOffset
					file.Seek(offset, 0)
					continue
				} else if res.Header.Get("Max-Request-Size") != "" {
					newChunkSize, _ := strconv.ParseInt(res.Header.Get("Max-Request-Size"), 10, 64)
					log.Println(fmt.Sprintf("Chunk size error: changing from %v to %v", chunkSize, newChunkSize))
					chunkSize = newChunkSize
					buffer = make([]byte, chunkSize)
					file.Seek(offset, 0)
					continue
				}

				return err
			}
			offset += int64(bytesread)
			log.Println(res.Status)
		}
	}

	return nil
}

// UploadVideo is a function responsible for uploading video
func UploadVideo(videoPath string) {
	ticker := time.NewTicker(defaultWaitingTime * time.Second)

	for trial := 0; trial <= defaultMaxRetries; trial, _ = trial+1, <-ticker.C {
		err := updateUploadURL()
		if err != nil {
			log.Println("Can't contact master")
			log.Println(err)
			updateMasterURL()
			continue
		}

		id, err := sendVideoInitialRequest(videoPath)
		if err != nil {
			log.Println("Can't connect to node")
			log.Println(err)
			continue
		}

		log.Println("Sent inital request with ID =", id)
		videoPathMap := map[string]string{
			"video": videoPath,
		}

		err = uploadFiles(id, videoPathMap, []string{"video"})
		if err == nil {
			log.Println("Upload successful")
			return
		}
	}
	log.Fatal("File not uploaded")
}

// UploadModel is a function responsible for uploading model
func UploadModel(modelPath string, configPath string, codePath string) {
	ticker := time.NewTicker(defaultWaitingTime * time.Second)

	for trial := 0; trial <= defaultMaxRetries; trial, _ = trial+1, <-ticker.C {
		err := updateUploadURL()
		if err != nil {
			log.Println("Can't contact master")
			log.Println(err)
			updateMasterURL()
			continue
		}

		modelID, err := sendModelInitialRequest(modelPath, configPath, codePath)
		if err != nil {
			log.Println("Can't connect to node")
			log.Println(err)
			continue
		}

		log.Println("Sent inital request for model with ID =", modelID)
		uploadFilesPaths := map[string]string{
			"model":  modelPath,
			"config": configPath,
			"code":   codePath,
		}
		err = uploadFiles(modelID, uploadFilesPaths, modelUploadOrder)
		if err != nil {
			log.Println(err)
			continue
		}

		log.Println("Upload successful")
		return
	}
	log.Fatal("File not uploaded")
}

func main() {
	mode := flag.String("mode", "", "Mode of operatoin (video/model)")
	videoPath := flag.String("video", "", "Path to video file")
	modelPath := flag.String("model", "", "Path to model file")
	configPath := flag.String("config", "", "Path to config file")
	codePath := flag.String("code", "", "Path to code file")
	flag.Parse()

	if len(flag.Args()) == 0 {
		log.Fatal("No masters ip provided")
	}
	mastersURLS = append(mastersURLS, flag.Args()...)
	updateMasterURL()
	
	switch *mode {
	case "model":
		if *modelPath == "" {
			log.Fatal("model flag wasn't provided")
		}
		if *configPath == "" {
			log.Fatal("config flag wasn't provided")
		}
		if *codePath == "" {
			log.Fatal("code flag wasn't provided")
		}
		UploadModel(*modelPath, *configPath, *codePath)
	case "video":
		if *videoPath == "" {
			log.Fatal("video flag wasn't provided")
		}
		UploadVideo(*videoPath)
	default:
		log.Fatal("Invalid mode")
	}

}
