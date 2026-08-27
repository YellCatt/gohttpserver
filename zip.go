package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	dkignore "github.com/codeskyblue/dockerignore"
	"golang.org/x/text/encoding/simplifiedchinese"
)

type Zip struct {
	*zip.Writer
}

func sanitizedName(filename string) string {
	if len(filename) > 1 && filename[1] == ':' &&
		runtime.GOOS == "windows" {
		filename = filename[2:]
	}
	filename = strings.TrimLeft(strings.Replace(filename, `\`, "/", -1), `/`)
	filename = filepath.ToSlash(filename)
	filename = filepath.Clean(filename)
	return filename
}

func statFile(filename string) (info os.FileInfo, reader io.ReadCloser, err error) {
	info, err = os.Lstat(filename)
	if err != nil {
		debugLog("statFile失败: %s 错误=%v", filename, err)
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		var target string
		target, err = os.Readlink(filename)
		if err != nil {
			debugLog("读取符号链接失败: %s 错误=%v", filename, err)
			return
		}
		reader = ioutil.NopCloser(bytes.NewBuffer([]byte(target)))
		debugLog("statFile: 符号链接 %s -> %s", filename, target)
	} else if !info.IsDir() {
		reader, err = os.Open(filename)
		if err != nil {
			debugLog("打开文件失败: %s 错误=%v", filename, err)
			return
		}
	} else {
		reader = ioutil.NopCloser(bytes.NewBuffer(nil))
	}
	return
}

func (z *Zip) Add(relpath, abspath string) error {
	info, rdc, err := statFile(abspath)
	if err != nil {
		return err
	}
	defer rdc.Close()

	hdr, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	hdr.Name = sanitizedName(relpath)
	if info.IsDir() {
		hdr.Name += "/"
	}
	hdr.Method = zip.Deflate // compress method
	writer, err := z.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(writer, rdc)
	return err
}

func CompressToZip(w http.ResponseWriter, rootDir string) {
	rootDir = filepath.Clean(rootDir)
	zipFileName := filepath.Base(rootDir) + ".zip"
	absRootDir, _ := filepath.Abs(rootDir)

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+zipFileName+`"`)

	debugLog("开始压缩目录: 绝对路径=%s 输出文件=%s", filepath.ToSlash(absRootDir), zipFileName)

	zw := &Zip{Writer: zip.NewWriter(w)}
	defer zw.Close()

	var fileCount int
	filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		zipPath := path[len(rootDir):]
		if info.Name() == YAMLCONF {
			debugLog("压缩时跳过.ghs.yml: %s", filepath.ToSlash(path))
			return nil
		}
		if err != nil {
			warnLog("压缩遍历出错: %s 错误=%v", filepath.ToSlash(path), err)
			return nil
		}
		fileCount++
		debugLog("压缩添加: %s", filepath.ToSlash(path))
		return zw.Add(zipPath, path)
	})
	debugLog("压缩完成: 文件数=%d", fileCount)
}

func ExtractFromZip(zipFile, path string, w io.Writer) (err error) {
	absZipFile, _ := filepath.Abs(zipFile)
	debugLog("从ZIP提取文件: 压缩包=%s 目标=%s", filepath.ToSlash(absZipFile), path)
	cf, err := zip.OpenReader(zipFile)
	if err != nil {
		errorLog("打开ZIP文件失败: 压缩包=%s 错误=%v", filepath.ToSlash(absZipFile), err)
		return
	}
	defer cf.Close()

	rd := ioutil.NopCloser(bytes.NewBufferString(path))
	patterns, err := dkignore.ReadIgnore(rd)
	if err != nil {
		errorLog("读取忽略规则失败: 路径=%s 错误=%v", path, err)
		return
	}

	for _, file := range cf.File {
		matched, _ := dkignore.Matches(file.Name, patterns)
		if !matched {
			continue
		}
		rc, er := file.Open()
		if er != nil {
			errorLog("打开ZIP内文件失败: 文件名=%s 错误=%v", file.Name, er)
			err = er
			return
		}
		defer rc.Close()
		_, err = io.Copy(w, rc)
		if err != nil {
			errorLog("复制ZIP内文件内容失败: 文件名=%s 错误=%v", file.Name, err)
			return
		}
		debugLog("提取ZIP文件成功: 文件名=%s", file.Name)
		return
	}
	errorLog("ZIP内未找到匹配文件: 目标=%s", path)
	return fmt.Errorf("文件 %s 未找到", strconv.Quote(path))
}

func unzipFile(filename, dest string) error {
	absFilename, _ := filepath.Abs(filename)
	absDest, _ := filepath.Abs(dest)
	infoLog("开始解压: 压缩包=%s 目标=%s", filepath.ToSlash(absFilename), filepath.ToSlash(absDest))

	zr, err := zip.OpenReader(filename)
	if err != nil {
		errorLog("打开ZIP文件失败: %s 错误=%v", filepath.ToSlash(absFilename), err)
		return err
	}
	defer zr.Close()

	if dest == "" {
		dest = filepath.Dir(filename)
	}

	var fileCount, dirCount int
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			errorLog("打开ZIP条目失败: 文件名=%s 错误=%v", f.Name, err)
			return err
		}
		defer rc.Close()

		filename := sanitizedName(f.Name)
		if filepath.Base(filename) == ".ghs.yml" {
			debugLog("解压时跳过.ghs.yml: %s", filename)
			continue
		}
		fpath := filepath.Join(dest, filename)

		if f.Flags&(1<<11) == 0 {
			tr := simplifiedchinese.GB18030.NewDecoder()
			fpathUtf8, err := tr.String(fpath)
			if err == nil {
				fpath = fpathUtf8
			}
		}

		absFpath, _ := filepath.Abs(fpath)

		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, os.ModePerm)
			dirCount++
			debugLog("解压创建目录: 绝对路径=%s", filepath.ToSlash(absFpath))
			continue
		}

		os.MkdirAll(filepath.Dir(fpath), os.ModePerm)
		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			errorLog("创建解压文件失败: 绝对路径=%s 错误=%v", filepath.ToSlash(absFpath), err)
			return err
		}
		_, err = io.Copy(outFile, rc)
		outFile.Close()

		if err != nil {
			errorLog("写入解压文件失败: 绝对路径=%s 错误=%v", filepath.ToSlash(absFpath), err)
			return err
		}
		fileCount++
		debugLog("解压文件: 绝对路径=%s 大小=%d", filepath.ToSlash(absFpath), f.UncompressedSize64)
	}
	infoLog("解压完成: 文件数=%d 目录数=%d 目标=%s", fileCount, dirCount, filepath.ToSlash(absDest))
	return nil
}