package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/cheggaaa/pb/v3"
)

func main() {
	var url, targetParentFolder, proxyURLHead string
	flag.StringVar(&url, "u", "", "huggingface url,such as: https://huggingface.co/datasets/Mizukiluke/ureader-instruction-1.0/tree/main")
	flag.StringVar(&targetParentFolder, "f", "./", "target folder")
	flag.StringVar(&proxyURLHead, "p", "https://worker-share-proxy-01f5.xieincz.tk/", "proxy url")
	flag.Parse()
	if url == "" {
		flag.Usage()
		return
	}

	// 提取文件名和链接
	// 使用 strings.TrimSuffix 函数去掉 "/tree/main"
	modelURL := strings.TrimSuffix(url, "/tree/main")
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

	// 拼接代理链接
	proxyURL := proxyURLHead + urlEncode(url)

	// 发起HTTP请求获取页面内容
	response, err := http.Get(proxyURL)
	if err != nil {
		fmt.Printf("cannot get page content: %v\n", err)
		return
	}
	defer response.Body.Close()

	// 使用goquery解析HTML页面
	document, err := goquery.NewDocumentFromReader(response.Body)
	if err != nil {
		fmt.Printf("cannot parse html: %v\n", err)
		return
	}

	// 定义选择器
	selector := "body > div > main > div.container.relative.flex.flex-col.md\\:grid.md\\:space-y-0.w-full.md\\:grid-cols-12.space-y-4.md\\:gap-6.mb-16 > section > div:nth-child(4) > ul"

	// 使用选择器选择所有的li:nth-child
	document.Find(selector).Find("li").Each(func(i int, li *goquery.Selection) {
		// 对每个li:nth-child进行操作
		spanText := li.Find("div > a > span").Text()
		fmt.Printf("downloading file %s\n", spanText)
		// 拼接文件下载链接
		fileURL := modelURL + "/resolve/main/" + spanText

		//拼接文件下载代理链接
		proxyFileURL := proxyURLHead + urlEncode(fileURL)
		// 下载文件并保存到目标文件夹
		if err := downloadFileWithProgressBar(proxyFileURL, path.Join(targetFolder, spanText)); err != nil {
			fmt.Printf("cannot download file %s: %v\n", spanText, err)
		}

	})
	fmt.Println("download task completed")
}

func downloadFile(url, filePath string) error {
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

	_, err = io.Copy(file, response.Body)
	if err != nil {
		return err
	}

	return nil
}

func downloadFileWithProgressBar(url, filePath string) error {
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

	contentLength := response.ContentLength
	if contentLength <= 0 {
		fmt.Printf("content length is not available but tried to download without progress bar, it should be fine\n")
		return downloadFile(url, filePath)
	}

	bar := pb.New(int(contentLength)).Set(pb.Bytes, true)
	bar.Start()

	reader := bar.NewProxyReader(response.Body)

	_, err = io.Copy(file, reader)
	if err != nil {
		return err
	}

	bar.Finish()
	return nil
}

func urlEncode(s string) string {
	return url.QueryEscape(s)
}
