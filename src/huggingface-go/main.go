package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cheggaaa/pb/v3"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
)

// --- Repo Type Definition ---
type RepoType string

const (
	RepoTypeModel   RepoType = "model"
	RepoTypeDataset RepoType = "dataset"
)

// --- Constants ---
const (
	defaultMirrorURL     = "https://hf-mirror.com"
	modelAPIPathPrefix   = "/api/models/"   // API prefix for models
	datasetAPIPathPrefix = "/api/datasets/" // API prefix for datasets
	resolvePathPrefix    = "/resolve/"
	treePathPrefix       = "/tree/"
	defaultBranch        = "main"
	maxRetries           = 5
	retryDelay           = 3 * time.Second
	defaultWorkerCount   = 8
	httpTimeout          = 30 * time.Minute
	rateLimit            = 10
	lfsFileThreshold     = 10 * 1024 * 1024
)

// --- Global HTTP Client ---
var httpClient = &http.Client{
	Timeout: httpTimeout,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 10,
	},
}

// FileEntry represents information about a file to be downloaded.
type FileEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Type string `json:"type"`
	URL  string
}

// Downloader encapsulates the download logic and configuration.
type Downloader struct {
	repoID          string
	repoType        RepoType // Added to distinguish between model and dataset
	branch          string
	targetSubFolder string
	localModelDir   string
	mirrorHost      string
	proxyPrefix     string
	workerCount     int
	filesToDownload []FileEntry
	totalSize       int64
	apiRateLimiter  *rate.Limiter
}

// getAPIPathPrefix is a helper to get the correct API path.
func (d *Downloader) getAPIPathPrefix() string {
	if d.repoType == RepoTypeDataset {
		return datasetAPIPathPrefix
	}
	return modelAPIPathPrefix
}

// getResolveBasePath is a helper to get the correct base path for file URLs.
func (d *Downloader) getResolveBasePath() string {
	if d.repoType == RepoTypeDataset {
		// For datasets, the path is "datasets/{repoID}"
		return "datasets/" + d.repoID
	}
	// For models, the path is just "{repoID}"
	return d.repoID
}

// NewDownloader creates a Downloader instance by parsing user input.
func NewDownloader(rawURL, targetParentFolder, proxyURLHead, mirrorURL string, disableDefaultMirror bool, workers int) (*Downloader, error) {
	rawURL = strings.TrimSuffix(rawURL, "/")

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	d := &Downloader{
		proxyPrefix:    proxyURLHead,
		workerCount:    workers,
		apiRateLimiter: rate.NewLimiter(rate.Limit(rateLimit), 1),
	}

	if disableDefaultMirror {
		d.mirrorHost = parsedURL.Scheme + "://" + parsedURL.Host
		fmt.Printf("Default mirror disabled, using %s as base URL\n", d.mirrorHost)
	} else {
		d.mirrorHost = mirrorURL
	}

	// Parse Repo ID, Branch, and Subfolder
	pathParts := strings.Split(strings.TrimPrefix(parsedURL.Path, "/"), "/")

	if len(pathParts) > 0 && pathParts[0] == "datasets" {
		d.repoType = RepoTypeDataset
		pathParts = pathParts[1:] // Slice off "datasets" part for subsequent parsing
		fmt.Println("Dataset repository detected.")
	} else {
		d.repoType = RepoTypeModel
		fmt.Println("Model repository detected.")
	}

	treeIndex := -1
	for i, part := range pathParts {
		if part == "tree" {
			treeIndex = i
			break
		}
	}

	if treeIndex == -1 {
		d.repoID = strings.Join(pathParts, "/")
		d.branch = defaultBranch
	} else {
		if treeIndex+1 >= len(pathParts) {
			return nil, errors.New("URL format error: missing branch name after /tree/")
		}
		d.repoID = strings.Join(pathParts[:treeIndex], "/")
		d.branch = pathParts[treeIndex+1]
		if treeIndex+2 < len(pathParts) {
			d.targetSubFolder = strings.Join(pathParts[treeIndex+2:], "/")
		}
	}

	modelName := path.Base(d.repoID)
	d.localModelDir = filepath.Join(targetParentFolder, modelName)

	return d, nil
}

// fetchFileListRecursive recursively fetches the file list using the Hugging Face API.
func (d *Downloader) fetchFileListRecursive(ctx context.Context, currentPath string) ([]FileEntry, error) {
	var entries []FileEntry

	apiURL, err := url.Parse(d.mirrorHost)
	if err != nil {
		return nil, fmt.Errorf("failed to parse mirror host: %w", err)
	}

	apiURL = apiURL.JoinPath(d.getAPIPathPrefix(), d.repoID, treePathPrefix, d.branch, currentPath)

	reqURL := d.proxyPrefix + apiURL.String()

	if err := d.apiRateLimiter.Wait(ctx); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create API request for %s: %w", reqURL, err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed for %s: %w", reqURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API request failed for %s with status code: %d", reqURL, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read API response body: %w", err)
	}

	var rawEntries []FileEntry
	if err := json.Unmarshal(body, &rawEntries); err != nil {
		return nil, fmt.Errorf("failed to parse JSON from %s: %w", reqURL, err)
	}

	for _, entry := range rawEntries {
		if entry.Type == "file" {
			if d.targetSubFolder == "" || strings.HasPrefix(entry.Path, d.targetSubFolder+"/") || entry.Path == d.targetSubFolder {
				fileURL, _ := url.Parse(d.mirrorHost)
				fileURL = fileURL.JoinPath(d.getResolveBasePath(), "resolve", d.branch, entry.Path)
				entry.URL = fileURL.String()
				entries = append(entries, entry)
			}
		} else if entry.Type == "directory" {
			subEntries, err := d.fetchFileListRecursive(ctx, entry.Path)
			if err != nil {
				return nil, err
			}
			entries = append(entries, subEntries...)
		}
	}

	return entries, nil
}

// Download starts the entire download process.
func (d *Downloader) Download(ctx context.Context) error {
	fmt.Println("Fetching file list... (This may take a moment)")
	allFiles, err := d.fetchFileListRecursive(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to fetch file list: %w", err)
	}
	d.filesToDownload = allFiles

	if len(d.filesToDownload) == 0 {
		fmt.Println("No files found. Please check the URL or the specified subfolder.")
		return nil
	}

	for _, file := range d.filesToDownload {
		d.totalSize += file.Size
	}

	convertedSize, unit := convertBytes(float64(d.totalSize))
	fmt.Printf("Name: %s\n", path.Base(d.repoID))
	if d.targetSubFolder != "" {
		fmt.Printf("Target Subfolder: %s\n", d.targetSubFolder)
	}
	fmt.Printf("Branch: %s\n", d.branch)
	fmt.Printf("Total files to download: %d\n", len(d.filesToDownload))
	fmt.Printf("Total file size: %.2f %s\n", convertedSize, unit)

	if err := os.MkdirAll(d.localModelDir, 0755); err != nil {
		return fmt.Errorf("could not create target folder: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(d.workerCount)

	totalBar := pb.New64(d.totalSize).Set(pb.Bytes, true).SetTemplateString(`{{ "Total Progress:" }} {{ bar . }} {{percent . }} {{speed . "%s/s"}} {{etime .}}`)

	pool, err := pb.StartPool(totalBar)
	if err != nil {
		return err
	}
	defer pool.Stop()

	for _, file := range d.filesToDownload {
		file := file
		g.Go(func() error {
			return d.processFileDownload(ctx, file, pool, totalBar)
		})
	}

	if err := g.Wait(); err != nil {
		fmt.Println()
		return err
	}

	fmt.Println("\nAll download tasks completed successfully!")
	return nil
}

// processFileDownload handles the download logic for a single file.
func (d *Downloader) processFileDownload(ctx context.Context, file FileEntry, pool *pb.Pool, totalBar *pb.ProgressBar) error {
	// file.Path already contains the correct relative path from the repo root (e.g., "des/config.json").
	// We join it directly with the local model directory to preserve the folder structure.
	localFilePath := filepath.Join(d.localModelDir, file.Path)

	if stat, err := os.Stat(localFilePath); err == nil {
		if stat.Size() == file.Size {
			totalBar.Add64(file.Size)
			return nil
		}
	}

	fileBar := pb.New64(file.Size).Set(pb.Bytes, true).SetTemplateString(fmt.Sprintf(`{{ "%s:" }} {{ bar . }} {{percent . }} {{speed . "%%s/s"}}`, path.Base(file.Path)))
	pool.Add(fileBar)

	err := d.downloadFileWithRetry(ctx, file, localFilePath, fileBar, totalBar)
	if err != nil {
		errorMsg := fmt.Sprintf("Failed to download %s after max retries: %v", file.Path, err)
		fileBar.SetTemplateString(fmt.Sprintf(`{{ "%s:" }} {{ "Download Failed" }}`, path.Base(file.Path))).Finish()
		return errors.New(errorMsg)
	}
	fileBar.Finish()
	return nil
}

// downloadFileWithRetry handles the download of a single file with retry logic.
func (d *Downloader) downloadFileWithRetry(ctx context.Context, file FileEntry, localPath string, bar, totalBar *pb.ProgressBar) error {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := d.downloadFileAtomically(ctx, file, localPath, bar, totalBar)
		if err == nil {
			return nil
		}
		lastErr = err

		delay := time.Duration(i*i)*time.Second + retryDelay
		bar.Set("prefix", fmt.Sprintf("Retry in %v... ", delay))
		time.Sleep(delay)
		bar.Set("prefix", fmt.Sprintf(`{{ "%s:" }}`, path.Base(file.Path)))
		bar.SetCurrent(0)
	}
	return lastErr
}

// downloadFileAtomically performs the actual file download and ensures atomic writes.
func (d *Downloader) downloadFileAtomically(ctx context.Context, file FileEntry, localPath string, bar, totalBar *pb.ProgressBar) error {
	var startOffset int64
	tmpPath := localPath + ".tmp"
	if stat, err := os.Stat(tmpPath); err == nil {
		startOffset = stat.Size()
	}

	reqURL := d.proxyPrefix + file.URL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	if startOffset > 0 {
		if startOffset == file.Size {
			totalBar.Add64(file.Size - bar.Current())
			bar.SetCurrent(file.Size)
			return os.Rename(tmpPath, localPath)
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startOffset))
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("request for %s failed with status: %s", file.URL, resp.Status)
	}

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	openFlags := os.O_CREATE | os.O_WRONLY
	if resp.StatusCode == http.StatusPartialContent {
		openFlags |= os.O_APPEND
	} else {
		openFlags |= os.O_TRUNC
		startOffset = 0
	}
	tmpFile, err := os.OpenFile(tmpPath, openFlags, 0644)
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer tmpFile.Close()

	bar.SetCurrent(startOffset)

	barReader := bar.NewProxyReader(resp.Body)
	totalBarReader := totalBar.NewProxyReader(barReader)

	_, err = io.Copy(tmpFile, totalBarReader)
	if err != nil {
		return fmt.Errorf("failed to write to file: %w", err)
	}

	if err := os.Rename(tmpPath, localPath); err != nil {
		return fmt.Errorf("failed to rename temporary file: %w", err)
	}

	return nil
}

// convertBytes converts bytes to a human-readable format.
func convertBytes(bytes float64) (float64, string) {
	const (
		KB = 1 << 10
		MB = 1 << 20
		GB = 1 << 30
	)
	switch {
	case bytes >= GB:
		return bytes / GB, "GB"
	case bytes >= MB:
		return bytes / MB, "MB"
	case bytes >= KB:
		return bytes / KB, "KB"
	default:
		return bytes, "B"
	}
}

func main() {
	var url, targetParentFolder, proxyURLHead, mirrorURL string
	var disableDefaultMirror bool
	var workerCount int

	flag.StringVar(&url, "u", "", "Hugging Face model/dataset URL (required)")
	flag.StringVar(&targetParentFolder, "f", "./", "Parent folder path to save the model")
	flag.StringVar(&proxyURLHead, "p", "", "Proxy URL prefix (optional)")
	flag.StringVar(&mirrorURL, "m", defaultMirrorURL, "Hugging Face mirror site URL")
	flag.BoolVar(&disableDefaultMirror, "d", false, "Disable the default mirror and use the domain from the -u parameter")
	flag.IntVar(&workerCount, "w", defaultWorkerCount, "Number of concurrent downloads")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s -u <model_or_dataset_url> [options]\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "\nExamples:")
		fmt.Fprintf(os.Stderr, "  %s -u https://huggingface.co/google-bert/bert-base-uncased\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -u https://huggingface.co/datasets/squad\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -u https://hf-mirror.com/core42/stable-diffusion-3-medium-diffusers/tree/main/text_encoder_3 -f D:/models\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "\nOptions:")
		flag.PrintDefaults()
	}

	flag.Parse()

	if url == "" {
		flag.Usage()
		return
	}

	downloader, err := NewDownloader(url, targetParentFolder, proxyURLHead, mirrorURL, disableDefaultMirror, workerCount)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to initialize downloader: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := downloader.Download(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: An error occurred during the download process: %v\n", err)
		os.Exit(1)
	}
}
