package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"flag"
	"os"
	"path"
	"strings"

	"path/filepath"

	"github.com/PuerkitoBio/goquery"
	"github.com/cheggaaa/pb/v3"
)

func main() {
	var url, targetParentFolder, proxyURLHead, homepage string
	flag.StringVar(&url, "u", "", "huggingface url,such as: https://huggingface.co/datasets/Mizukiluke/ureader-instruction-1.0")
	flag.StringVar(&targetParentFolder, "f", "./", "path to your target folder")
	flag.StringVar(&proxyURLHead, "p", "https://worker-share-proxy-01f5.xieincz.tk/", "proxy url")
	flag.StringVar(&homepage, "homepage", "https://github.com/xieincz/huggingface-go", "Homepage URL")
	flag.Parse()

	if url == "" {
		flag.Usage()
		return
	}
	if !strings.HasPrefix(url, "https://huggingface.co/") {
		fmt.Printf("invalid url: %s\n", url)
		return
	}
	if !strings.HasSuffix(proxyURLHead, "/") {
		proxyURLHead += "/"
	}

	// 提取文件名和链接
	// 使用 strings.TrimSuffix 函数去掉 "/tree/main"
	modelURL := strings.TrimSuffix(url, "/tree/main/")
	modelURL = strings.TrimSuffix(modelURL, "/tree/main")
	modelURL = strings.TrimSuffix(modelURL, "/")
	modelName := path.Base(modelURL)
	fmt.Printf("model/datasets name: %s\n", modelName)
	fmt.Printf("model/datasets url: %s\n", modelURL)

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
	entries, err := fetchDirectoryEntriesRecursively(proxyURLHead, modelURL+"/tree/main", "")
	if err != nil {
		fmt.Printf("cannot fetch entries: %v\n", err)
		return
	}
	for _, entry := range entries {
		// 获取文件路径
		filePath := entry["path"].(string)
		fmt.Printf("downloading file %s\n", filePath)
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
		fileURL := modelURL + "/resolve/main/" + entry["path"].(string)
		//拼接文件下载代理链接
		proxyFileURL := proxyURLHead + urlEncode(fileURL)
		// 下载文件并保存到目标文件夹
		if err := downloadFileWithProgressBar(proxyFileURL, filePath, int(entry["size"].(float64))); err != nil {
			fmt.Printf("cannot download file %s: %v\n", filePath, err)
		}

	}
	fmt.Println("download task completed")
}

func fetchDirectoryEntriesRecursively(proxyURLHead, baseURL, path string) ([]map[string]interface{}, error) {
	res := make([]map[string]interface{}, 0)
	url := baseURL
	if path != "" {
		url += "/" + path
	}
	proxyURL := proxyURLHead + urlEncode(url)
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

	entries, err := extractEntries(dataProps)
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

func urlEncode(s string) string {
	return url.QueryEscape(encode(s))
}

// 将url中的其他语言的字符转义
var replaceDic = map[string]string{
	"%2D": "-",
	"%5F": "_",
	"%2E": ".",
	"%21": "!",
	"%7E": "~",
	"%2A": "*",
	"%27": "'",
	"%28": "(",
	"%29": ")",
	"%3B": ";",
	"%3A": ":",
	"%40": "@",
	"%26": "&",
	"%3D": "=",
	"%2B": "+",
	"%24": "$",
	"%2C": ",",
	"%2F": "/",
	"%3F": "?",
	"%23": "#",
	"%25": "%",
}

func encode(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		fmt.Println("Error parsing URL: ", err)
		return ""
	}
	u.Path = url.PathEscape(u.Path)
	for k, v := range replaceDic {
		u.Path = strings.ReplaceAll(u.Path, k, v)
	}
	return strings.ReplaceAll(u.String(), "%25", "%")
}

func extractEntries(dataProps string) ([]map[string]interface{}, error) {
	var props map[string]interface{}
	err := json.Unmarshal([]byte(dataProps), &props)
	if err != nil {
		return nil, err
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
