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

// --- Constants ---
const (
	defaultMirrorURL   = "https://hf-mirror.com"
	apiPathPrefix      = "/api/models/"
	resolvePathPrefix  = "/resolve/"
	treePathPrefix     = "/tree/"
	defaultBranch      = "main"
	maxRetries         = 5                // Increased retries for better resilience
	retryDelay         = 3 * time.Second  // Slightly longer initial delay
	defaultWorkerCount = 8                // Default concurrent workers
	httpTimeout        = 30 * time.Minute // HTTP request timeout
	rateLimit          = 10               // Limit API requests to 10 per second
	lfsFileThreshold   = 10 * 1024 * 1024 // 10MB threshold for LFS files
)

// --- Global HTTP Client ---
// Using a shared transport allows for better connection reuse.
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
	branch          string
	targetSubFolder string // User-specified subfolder to download
	localModelDir   string // Local root directory for saving
	mirrorHost      string
	proxyPrefix     string
	workerCount     int
	filesToDownload []FileEntry
	totalSize       int64
	apiRateLimiter  *rate.Limiter
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

	// Determine the Hugging Face domain to use (mirror or official)
	if disableDefaultMirror {
		d.mirrorHost = parsedURL.Scheme + "://" + parsedURL.Host
		fmt.Printf("Default mirror disabled, using %s as base URL\n", d.mirrorHost)
	} else {
		d.mirrorHost = mirrorURL
	}

	// Parse Repo ID, Branch, and Subfolder
	pathParts := strings.Split(strings.TrimPrefix(parsedURL.Path, "/"), "/")
	treeIndex := -1
	for i, part := range pathParts {
		if part == "tree" {
			treeIndex = i
			break
		}
	}

	if treeIndex == -1 { // URL does not contain /tree/
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
	apiURL = apiURL.JoinPath(apiPathPrefix, d.repoID, treePathPrefix, d.branch, currentPath)
	reqURL := d.proxyPrefix + apiURL.String()

	// Wait for the rate limiter
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
			// If a subfolder is specified, only include files within that subfolder.
			if d.targetSubFolder == "" || strings.HasPrefix(entry.Path, d.targetSubFolder+"/") || entry.Path == d.targetSubFolder {
				fileURL, _ := url.Parse(d.mirrorHost)
				fileURL = fileURL.JoinPath(d.repoID, "resolve", d.branch, entry.Path)
				entry.URL = fileURL.String()
				entries = append(entries, entry)
			}
		} else if entry.Type == "directory" {
			subEntries, err := d.fetchFileListRecursive(ctx, entry.Path)
			if err != nil {
				return nil, err // Propagate errors upwards
			}
			entries = append(entries, subEntries...)
		}
	}

	return entries, nil
}

// Download starts the entire download process.
func (d *Downloader) Download(ctx context.Context) error {
	fmt.Println("Fetching file list... (This may take a moment)")
	// Always start fetching from the repository root and filter during inclusion.
	allFiles, err := d.fetchFileListRecursive(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to fetch file list: %w", err)
	}
	d.filesToDownload = allFiles

	if len(d.filesToDownload) == 0 {
		fmt.Println("No files found. Please check the URL or the specified subfolder.")
		return nil
	}

	// Calculate total size
	for _, file := range d.filesToDownload {
		d.totalSize += file.Size
	}

	convertedSize, unit := convertBytes(float64(d.totalSize))
	fmt.Printf("Model/Dataset Name: %s\n", path.Base(d.repoID))
	if d.targetSubFolder != "" {
		fmt.Printf("Target Subfolder: %s\n", d.targetSubFolder)
	}
	fmt.Printf("Branch: %s\n", d.branch)
	fmt.Printf("Total files to download: %d\n", len(d.filesToDownload))
	fmt.Printf("Total file size: %.2f %s\n", convertedSize, unit)

	// Create local model directory
	if err := os.MkdirAll(d.localModelDir, 0755); err != nil {
		return fmt.Errorf("could not create target folder: %w", err)
	}

	// --- Concurrent Downloading ---
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(d.workerCount)

	totalBar := pb.New64(d.totalSize).Set(pb.Bytes, true).SetTemplateString(`{{ "Total Progress:" }} {{ bar . }} {{percent . }} {{speed . "%s/s"}} {{etime .}}`)
	// FIX: Do NOT call totalBar.Start() here. The pool will manage it.

	pool, err := pb.StartPool(totalBar)
	if err != nil {
		return err
	}
	defer pool.Stop()

	for _, file := range d.filesToDownload {
		file := file // Capture loop variable
		g.Go(func() error {
			return d.processFileDownload(ctx, file, pool, totalBar)
		})
	}

	if err := g.Wait(); err != nil {
		// The final totalBar.Finish() will not be called on error, so we print a newline
		// to ensure the next shell prompt starts on a new line.
		fmt.Println()
		return err
	}

	fmt.Println("\nAll download tasks completed successfully!")
	return nil
}

// processFileDownload handles the download logic for a single file.
func (d *Downloader) processFileDownload(ctx context.Context, file FileEntry, pool *pb.Pool, totalBar *pb.ProgressBar) error {
	localRelativePath := file.Path
	if d.targetSubFolder != "" {
		localRelativePath = strings.TrimPrefix(file.Path, d.targetSubFolder+"/")
	}
	localFilePath := filepath.Join(d.localModelDir, localRelativePath)

	// Check if the file exists and has the correct size
	if stat, err := os.Stat(localFilePath); err == nil {
		if stat.Size() == file.Size {
			// Use a mutex or a synchronized print function if this output gets garbled,
			// but for simple infrequent messages like this, it's often fine.
			// fmt.Printf("File %s already exists with the same size, skipping.\n", localRelativePath)
			totalBar.Add64(file.Size) // Update total progress even if skipped
			return nil
		}
	}

	// Note: The double '%%' is necessary to escape the percent sign for fmt.Sprintf.
	fileBar := pb.New64(file.Size).Set(pb.Bytes, true).SetTemplateString(fmt.Sprintf(`{{ "%s:" }} {{ bar . }} {{percent . }} {{speed . "%%s/s"}}`, path.Base(file.Path)))
	pool.Add(fileBar)
	// We don't defer fileBar.Finish() here because the pool manages its lifecycle.

	err := d.downloadFileWithRetry(ctx, file, localFilePath, fileBar, totalBar)
	if err != nil {
		errorMsg := fmt.Sprintf("Failed to download %s after max retries: %v", file.Path, err)
		fileBar.SetTemplateString(fmt.Sprintf(`{{ "%s:" }} {{ "Download Failed" }}`, path.Base(file.Path))).Finish()
		// Return a new error to avoid race conditions on the original error
		return errors.New(errorMsg)
	}
	fileBar.Finish()
	return nil
}

// downloadFileWithRetry handles the download of a single file with retry logic.
func (d *Downloader) downloadFileWithRetry(ctx context.Context, file FileEntry, localPath string, bar, totalBar *pb.ProgressBar) error {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		// Check for context cancellation before each attempt
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := d.downloadFileAtomically(ctx, file, localPath, bar, totalBar)
		if err == nil {
			return nil // Success
		}
		lastErr = err

		// Exponential backoff for retries
		delay := time.Duration(i*i)*time.Second + retryDelay
		// Using the bar's writer to print the message ensures it doesn't mess up the progress bar rendering
		bar.Set("prefix", fmt.Sprintf("Retry in %v... ", delay))
		time.Sleep(delay)
		bar.Set("prefix", fmt.Sprintf(`{{ "%s:" }}`, path.Base(file.Path))) // Reset prefix
		bar.SetCurrent(0)                                                   // Reset progress bar for retry
	}
	return lastErr
}

// downloadFileAtomically performs the actual file download and ensures atomic writes.
func (d *Downloader) downloadFileAtomically(ctx context.Context, file FileEntry, localPath string, bar, totalBar *pb.ProgressBar) error {
	// Determine the starting point for a potential resume
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

	// Set the Range header if we are resuming a download
	if startOffset > 0 {
		// If the existing temp file is already complete, we can skip the download
		if startOffset == file.Size {
			// We still need to update the total progress bar
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

	// Handle the response status code
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("request for %s failed with status: %s", file.URL, resp.Status)
	}

	// Create local directories
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Open the file for writing (append if resuming)
	openFlags := os.O_CREATE | os.O_WRONLY
	if resp.StatusCode == http.StatusPartialContent {
		openFlags |= os.O_APPEND
	} else {
		// If not resuming, truncate the file
		openFlags |= os.O_TRUNC
		startOffset = 0 // Reset offset just in case
	}
	tmpFile, err := os.OpenFile(tmpPath, openFlags, 0644)
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer tmpFile.Close()

	// Set the initial progress for the bars
	bar.SetCurrent(startOffset)
	// The total bar is updated via the proxy reader, so no need to set it manually here.

	// Create a proxy reader to update progress bars
	barReader := bar.NewProxyReader(resp.Body)
	totalBarReader := totalBar.NewProxyReader(barReader)

	// Copy data to the temporary file
	_, err = io.Copy(tmpFile, totalBarReader)
	if err != nil {
		return fmt.Errorf("failed to write to file: %w", err)
	}

	// Rename the temporary file to the final destination
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
		fmt.Fprintf(os.Stderr, "Usage: %s -u <model_url> [options]\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "Examples:")
		fmt.Fprintf(os.Stderr, "  %s -u https://huggingface.co/google-bert/bert-base-uncased\n", os.Args[0])
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

	// Use a context to handle graceful shutdown (e.g., on Ctrl+C)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// You can add a signal handler here to call cancel() on interrupt

	if err := downloader.Download(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: An error occurred during the download process: %v\n", err)
		os.Exit(1)
	}
}
