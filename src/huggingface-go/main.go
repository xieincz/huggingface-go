package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"flag"
	"os"
	"path"
	"strings"

	"path/filepath"

	"github.com/PuerkitoBio/goquery"
	"github.com/cheggaaa/pb/v3"
)

var huggingfaceHead string

func main() {
	var url, targetParentFolder, proxyURLHead, homepage string
	flag.StringVar(&url, "u", "", "huggingface url,such as: https://hf-mirror.com/Finnish-NLP/t5-large-nl36-finnish/tree/main")
	flag.StringVar(&targetParentFolder, "f", "./", "path to your target folder")
	flag.StringVar(&proxyURLHead, "p", "", "proxy url, leave it empty if you don't need it")
	flag.StringVar(&homepage, "homepage", "https://github.com/xieincz/huggingface-go", "Homepage URL")
	flag.Parse()

	if url == "" {
		flag.Usage()
		return
	}

	// 提取文件名和链接
	modelURL := strings.Split(url, "/tree/")[0]
	branch := strings.Split(strings.Split(url, "/tree/")[1], "/")[0] //需要输入url必须含branch，否则后面会出问题
	modelName := path.Base(modelURL)
	tmp := strings.Split(url, branch+"/") //需要输入url末尾不含/，否则后面会出问题
	var urlFolder string
	if len(tmp) < 2 {
		urlFolder = ""
	} else {
		urlFolder = tmp[1]
	}

	//提取出域名
	tmp = strings.Split(url, "/")
	huggingfaceHead = tmp[0] + "//" + tmp[2] //e.g. https://huggingface.co

	fmt.Printf("model/datasets name: %s\n", modelName)
	fmt.Printf("model/datasets url: %s\n", modelURL)
	fmt.Printf("branch: %s\n", branch)

	// 创建目标文件夹
	targetFolder := path.Join(targetParentFolder, modelName)
	if _, err := os.Stat(targetFolder); err == nil {
		fmt.Printf("target folder %s already exists\n", targetFolder)
		return
	}
	if err := os.MkdirAll(targetFolder, 0755); err != nil {
		fmt.Printf("cannot create target folder: %v\n", err)
		return
	}
	// 递归获取文件列表
	fmt.Println("fetching file list... \nthis may take a while")
	entries, err := fetchDirectoryEntriesRecursively(proxyURLHead, modelURL+"/tree/"+branch, urlFolder)
	if err != nil {
		fmt.Printf("cannot fetch entries: %v\n", err)
		return
	}
	totalFileSize := 0.0
	fileCount := 0
	for _, entry := range entries {
		totalFileSize += entry["size"].(float64)
		fileCount += 1
	}
	fmt.Printf("Total number of files: %d\n", fileCount)
	convertedSize, unit := convertBytes(totalFileSize)
	fmt.Printf("Total size of files: %.2f %s\n", convertedSize, unit)
	cnt := 1
	for _, entry := range entries {
		// 获取文件路径
		filePath := entry["path"].(string)
		fmt.Printf("downloading file %d/%d: %s\n", cnt, fileCount, filePath)
		cnt += 1
		filePath = path.Join(targetFolder, filePath)
		// 获取文件夹路径
		dirPath := filepath.Dir(filePath)
		// 检查文件夹是否存在，如果不存在则创建它
		if _, err := os.Stat(dirPath); os.IsNotExist(err) {
			err := os.MkdirAll(dirPath, os.ModePerm)
			if err != nil {
				fmt.Println("Error creating directory:", err)
				return
			}
		}
		// 拼接文件下载链接
		fileURL := modelURL + "/resolve/" + branch + "/" + entry["path"].(string)
		//拼接文件下载代理链接
		proxyFileURL := proxyURLHead + fileURL
		// 下载文件并保存到目标文件夹
		if err := downloadFileWithProgressBar(proxyFileURL, filePath, int(entry["size"].(float64))); err != nil {
			fmt.Printf("cannot download file %s: %v\n", filePath, err)
		}

	}
	fmt.Println("download task completed")
}

// Helper function to convert Bytes to appropriate unit
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

func fetchDirectoryEntriesRecursively(proxyURLHead, baseURL, path string) ([]map[string]interface{}, error) {
	res := make([]map[string]interface{}, 0)
	url := baseURL
	if path != "" {
		url += "/" + path
	}
	proxyURL := proxyURLHead + url
	response, err := http.Get(proxyURL)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	document, err := goquery.NewDocumentFromReader(response.Body)
	if err != nil {
		return nil, err
	}

	selection := document.Find("body > div > main > div.container.relative.flex.flex-col.md\\:grid.md\\:space-y-0.w-full.md\\:grid-cols-12.space-y-4.md\\:gap-6.mb-16 > section > div:nth-child(4)")

	dataProps, exists := selection.Attr("data-props")
	if !exists {
		fmt.Println("current url:", url)
		fmt.Println("current proxy url:", proxyURL)
		return nil, fmt.Errorf("data-props attribute not found")
	}

	entries, err := extractEntries(dataProps, proxyURLHead)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry["type"] == "file" {
			res = append(res, entry)
		} else if entry["type"] == "directory" {
			subDirEntries, err := fetchDirectoryEntriesRecursively(proxyURLHead, baseURL, entry["path"].(string))
			if err != nil {
				return nil, err
			}
			res = append(res, subDirEntries...)
		} else {
			fmt.Println("Unconsidered file type:", entry["type"])
		}
	}

	return res, nil
}

func extractEntries(dataProps, proxyURLHead string) ([]map[string]interface{}, error) {
	var props map[string]interface{}
	err := json.Unmarshal([]byte(dataProps), &props)
	if err != nil {
		return nil, err
	}

	nextURL := props["nextURL"]
	fmt.Println("nextURL:", nextURL)
	if nextURL != nil {
		proxyURL := proxyURLHead + huggingfaceHead + nextURL.(string)
		response, err := http.Get(proxyURL)
		if err != nil {
			fmt.Println("Error:", err)
			return nil, err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			fmt.Println("Error reading response body:", err)
			return nil, err
		}
		// 解析JSON响应
		var data []interface{}
		if err := json.Unmarshal(body, &data); err != nil {
			fmt.Println("Error decoding JSON:", err)
			return nil, err
		}
		dataMaps := make([]map[string]interface{}, len(data))
		for i, v := range data {
			dataMap, ok := v.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("v is not a valid object")
			}
			dataMaps[i] = dataMap
		}
		return dataMaps, nil
	}

	entriesValue, exists := props["entries"]
	if !exists {
		return nil, fmt.Errorf("entries not found in data-props")
	}
	entries, ok := entriesValue.([]interface{})
	if !ok {
		return nil, fmt.Errorf("entries is not a valid array")
	}
	entryMaps := make([]map[string]interface{}, len(entries))
	for i, entry := range entries {
		entryMap, ok := entry.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("entry is not a valid object")
		}
		entryMaps[i] = entryMap
	}
	return entryMaps, nil
}

func downloadFileWithProgressBar(url, filePath string, fileSize int) error {
	response, err := http.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	file, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	bar := pb.New(int(fileSize)).Set(pb.Bytes, true)
	bar.Start()

	reader := bar.NewProxyReader(response.Body)

	_, err = io.Copy(file, reader)
	if err != nil {
		return err
	}

	bar.Finish()
	return nil
}
