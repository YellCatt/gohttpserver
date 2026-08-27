package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"regexp"

	"github.com/go-yaml/yaml"
	"github.com/gorilla/mux"
	"github.com/shogo82148/androidbinary/apk"
)

const YAMLCONF = ".ghs.yml"

const contentSecurityPolicy = "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; media-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'self'; object-src 'none'; frame-ancestors 'none'"

type ApkInfo struct {
	PackageName  string `json:"packageName"`
	MainActivity string `json:"mainActivity"`
	Version      struct {
		Code int    `json:"code"`
		Name string `json:"name"`
	} `json:"version"`
}

type IndexFileItem struct {
	Path string
	Info os.FileInfo
}

type Directory struct {
	size  map[string]int64
	mutex *sync.RWMutex
}

type HTTPStaticServer struct {
	Root             string
	Prefix           string
	Upload           bool
	Delete           bool
	Title            string
	Theme            string
	PlistProxy       string
	GoogleTrackerID  string
	AuthType         string
	DeepPathMaxDepth int
	NoIndex          bool

	indexes []IndexFileItem
	m       *mux.Router
	bufPool sync.Pool // use sync.Pool caching buf to reduce gc ratio
}

func NewHTTPStaticServer(root string, noIndex bool) *HTTPStaticServer {
	// if root == "" {
	// 	root = "./"
	// }
	// root = filepath.ToSlash(root)
	root = filepath.ToSlash(filepath.Clean(root))
	if !strings.HasSuffix(root, "/") {
		root = root + "/"
	}
	absRoot, _ := filepath.Abs(root)
	infoLog("根目录: %s (绝对路径: %s)", root, filepath.ToSlash(absRoot))
	m := mux.NewRouter()
	s := &HTTPStaticServer{
		Root:  root,
		Theme: "black",
		m:     m,
		bufPool: sync.Pool{
			New: func() interface{} { return make([]byte, 32*1024) },
		},
		NoIndex: noIndex,
	}

	debugLog("HTTP静态服务器初始化: 根目录=%s 索引构建=%v", root, !noIndex)

	if !noIndex {
		go func() {
			time.Sleep(1 * time.Second)
			for {
				startTime := time.Now()
				infoLog("开始构建搜索索引...")
				s.makeIndex()
				infoLog("搜索索引构建完成，耗时: %v", time.Since(startTime))
				time.Sleep(time.Minute * 10)
			}
		}()
	}

	m.HandleFunc("/-/ipa/plist/{path:.*}", s.hPlist)
	m.HandleFunc("/-/ipa/link/{path:.*}", s.hIpaLink)
	m.HandleFunc("/-/video-player/{path:.*}", s.hVideoPlayer)
	debugLog("已注册特殊路由: /-/ipa/plist /-/ipa/link /-/video-player")

	m.HandleFunc("/{path:.*}", s.hIndex).Methods("GET", "HEAD")
	m.HandleFunc("/{path:.*}", s.hUploadOrMkdir).Methods("POST")
	m.HandleFunc("/{path:.*}", s.hDelete).Methods("DELETE")
	debugLog("已注册通配路由: GET/HEAD->hIndex POST->hUploadOrMkdir DELETE->hDelete")
	return s
}

func (s *HTTPStaticServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	debugLog("收到请求: 方法=%s URL=%s 远程地址=%s", r.Method, r.URL.String(), r.RemoteAddr)
	defer func() {
		if rec := recover(); rec != nil {
			errorLog("请求处理panic: 方法=%s URL=%s 错误=%v", r.Method, r.URL.String(), rec)
		}
	}()
	s.m.ServeHTTP(w, r)
}

// Return real path with Seperator(/)
func (s *HTTPStaticServer) getRealPath(r *http.Request) string {
	path := mux.Vars(r)["path"]
	debugLog("getRealPath: 原始path变量=%s", path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	path = filepath.Clean(path)

	var relativePath string
	if s.Prefix == "" {
		relativePath = path
	} else {
		var err error
		relativePath, err = filepath.Rel(s.Prefix, path)
		if err != nil {
			warnLog("getRealPath Rel计算失败: prefix=%s path=%s 错误=%v", s.Prefix, path, err)
			relativePath = path
		}
	}

	realPath := filepath.Join(s.Root, relativePath)
	realPath = filepath.ToSlash(realPath)
	debugLog("getRealPath: prefix=%s cleanedPath=%s relativePath=%s realPath=%s", s.Prefix, path, relativePath, realPath)
	return realPath
}

func (s *HTTPStaticServer) absPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		warnLog("绝对路径转换失败: 原始=%s 错误=%v", p, err)
		return p
	}
	return filepath.ToSlash(abs)
}

func (s *HTTPStaticServer) hIndex(w http.ResponseWriter, r *http.Request) {
	path := mux.Vars(r)["path"]
	if strings.HasPrefix(path, "/-/") {
		warnLog("hIndex收到系统接口请求(可能路由未匹配): path=%s url=%s", path, r.URL.String())
	}
	realPath := s.getRealPath(r)
	absPath := s.absPath(realPath)

	if r.FormValue("json") == "true" {
		debugLog("列出目录JSON: uri=%s 绝对路径=%s", path, absPath)
		s.hJSONList(w, r)
		return
	}

	if r.FormValue("op") == "info" {
		debugLog("查询文件信息: uri=%s 绝对路径=%s", path, absPath)
		s.hInfo(w, r)
		return
	}

	if r.FormValue("op") == "archive" {
		debugLog("打包下载: uri=%s 绝对路径=%s", path, absPath)
		s.hZip(w, r)
		return
	}

	dir := isDir(realPath)
	fi, statErr := os.Stat(realPath)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			warnLog("路径不存在: uri=%s 绝对路径=%s 错误=%v", path, absPath, statErr)
			http.Error(w, "路径不存在", http.StatusNotFound)
			return
		}
		errorLog("路径状态检查失败: uri=%s 绝对路径=%s 错误=%v", path, absPath, statErr)
		http.Error(w, statErr.Error(), http.StatusInternalServerError)
		return
	}

	debugLog("请求详情: 方法=%s URI=%s 绝对路径=%s 是否目录=%v 文件大小=%d 文件名=%s 修改时间=%s",
		r.Method, path, absPath, dir, fi.Size(), fi.Name(), fi.ModTime().Format("2006-01-02 15:04:05"))

	if r.FormValue("raw") == "false" || dir {
		if r.Method == "HEAD" {
			debugLog("HEAD请求，直接返回: %s", absPath)
			return
		}
		debugLog("返回目录列表页面: %s", absPath)
		renderHTML(w, "assets/index.html", s)
	} else {
		if filepath.Base(path) == YAMLCONF {
			debugLog("检测到访问.ghs.yml配置文件，进行权限检查")
			auth := s.readAccessConf(realPath)
			if !auth.Delete {
				warnLog("尝试访问受限配置文件被拒绝: %s", absPath)
				http.Error(w, "安全警告，禁止读取此文件", http.StatusForbidden)
				return
			}
		}
		if r.FormValue("download") == "true" {
			debugLog("下载文件: %s 大小=%d", absPath, fi.Size())
			w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(filepath.Base(path)))
		} else {
			debugLog("返回文件内容: %s 大小=%d MIME类型=%s", absPath, fi.Size(), mime.TypeByExtension(filepath.Ext(path)))
		}
		http.ServeFile(w, r, realPath)
	}
}

func (s *HTTPStaticServer) hDelete(w http.ResponseWriter, req *http.Request) {
	path := mux.Vars(req)["path"]
	realPath := s.getRealPath(req)
	absPath := s.absPath(realPath)
	debugLog("删除请求: uri=%s 绝对路径=%s", path, absPath)

	fi, err := os.Stat(realPath)
	if err != nil {
		errorLog("删除前检查失败: uri=%s 绝对路径=%s 错误=%v", path, absPath, err)
		if os.IsNotExist(err) {
			http.Error(w, "路径不存在", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	if fi.IsDir() {
		debugLog("删除目录: %s 文件数约为=%d", absPath, countDirFiles(realPath))
	} else {
		debugLog("删除文件: %s 大小=%d", absPath, fi.Size())
	}

	auth := s.readAccessConf(realPath)
	if !auth.canDelete(req) {
		warnLog("删除被拒绝(权限不足): uri=%s 绝对路径=%s", path, absPath)
		http.Error(w, "禁止删除", http.StatusForbidden)
		return
	}

	infoLog("开始删除: uri=%s 绝对路径=%s", path, absPath)
	err = os.RemoveAll(realPath)
	if err != nil {
		errorLog("删除失败: uri=%s 绝对路径=%s 错误=%v", path, absPath, err)
		pathErr, ok := err.(*os.PathError)
		if ok {
			http.Error(w, pathErr.Op+" "+path+": "+pathErr.Err.Error(), 500)
		} else {
			http.Error(w, err.Error(), 500)
		}
		return
	}
	infoLog("删除完成: uri=%s 绝对路径=%s", path, absPath)
	w.Write([]byte("删除成功"))
}

func countDirFiles(dir string) int {
	count := 0
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		count++
		return nil
	})
	return count
}

func (s *HTTPStaticServer) hUploadOrMkdir(w http.ResponseWriter, req *http.Request) {
	dirpath := s.getRealPath(req)
	absDirpath := s.absPath(dirpath)
	debugLog("上传/建目录请求: 方法=%s 目录=%s 绝对路径=%s 内容类型=%s", req.Method, dirpath, absDirpath, req.Header.Get("Content-Type"))

	auth := s.readAccessConf(dirpath)
	if !auth.canUpload(req) {
		warnLog("上传被拒绝(权限不足): 绝对路径=%s", absDirpath)
		http.Error(w, "禁止上传", http.StatusForbidden)
		return
	}

	debugLog("权限检查通过: 上传权限=%v 删除权限=%v", auth.Upload, auth.Delete)

	if _, err := os.Stat(dirpath); os.IsNotExist(err) {
		debugLog("目录不存在，准备创建: %s", absDirpath)
		if err := os.MkdirAll(dirpath, os.ModePerm); err != nil {
			errorLog("创建目录失败: 绝对路径=%s 错误=%v", absDirpath, err)
			http.Error(w, "目录创建失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		infoLog("创建目录: 绝对路径=%s", absDirpath)
	}

	reader, err := req.MultipartReader()
	if err != nil {
		debugLog("非multipart请求，可能是仅创建目录: %s", absDirpath)
		w.Header().Set("Content-Type", "application/json;charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":     true,
			"destination": dirpath,
		})
		return
	}

	var filePart *multipart.Part
	var fileFallbackName string
	var filenameOverride string
	var unzipFlag bool

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			errorLog("读取multipart part失败: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		switch part.FormName() {
		case "file":
			fileFallbackName = part.FileName()
			filePart = part
		case "filename":
			buf, _ := io.ReadAll(part)
			filenameOverride = string(buf)
			part.Close()
		case "unzip":
			buf, _ := io.ReadAll(part)
			unzipFlag = (string(buf) == "true")
			part.Close()
		default:
			part.Close()
		}
	}

	if filePart == nil {
		debugLog("仅创建目录请求，无文件上传: %s", absDirpath)
		w.Header().Set("Content-Type", "application/json;charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":     true,
			"destination": dirpath,
		})
		return
	}
	defer filePart.Close()

	filename := filenameOverride
	if filename == "" {
		filename = fileFallbackName
	}
	debugLog("上传文件信息: 原始文件名=%s 目标文件名=%s", fileFallbackName, filename)

	if err := checkFilename(filename); err != nil {
		errorLog("文件名不合法: %s 错误=%v", filename, err)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	dstPath := filepath.Join(dirpath, filename)
	absDstPath := s.absPath(dstPath)
	_, fileExistErr := os.Stat(dstPath)
	isModify := fileExistErr == nil

	if isModify {
		infoLog("开始修改文件: 文件名=%s 绝对路径=%s", filename, absDstPath)
	} else {
		infoLog("开始创建文件: 文件名=%s 绝对路径=%s", filename, absDstPath)
	}

	dst, err := os.Create(dstPath)
	if err != nil {
		errorLog("创建文件失败: 绝对路径=%s 错误=%v", absDstPath, err)
		http.Error(w, "文件创建失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	buf := s.bufPool.Get().([]byte)
	defer s.bufPool.Put(buf)
	written, copyErr := io.CopyBuffer(dst, filePart, buf)
	if copyErr != nil {
		errorLog("写入文件失败: 绝对路径=%s 已写入=%d 错误=%v", absDstPath, written, copyErr)
		os.Remove(dstPath)
		http.Error(w, copyErr.Error(), http.StatusInternalServerError)
		return
	}

	infoLog("文件操作完成: 文件名=%s 大小=%d 绝对路径=%s", filename, written, absDstPath)

	w.Header().Set("Content-Type", "application/json;charset=utf-8")

	if unzipFlag {
		infoLog("开始解压: 源=%s 目标目录=%s", absDstPath, absDirpath)
		err = unzipFile(dstPath, dirpath)
		os.Remove(dstPath)
		message := "成功"
		if err != nil {
			errorLog("解压失败: 源=%s 目标目录=%s 错误=%v", absDstPath, absDirpath, err)
			message = err.Error()
		} else {
			infoLog("解压完成: 源=%s 目标目录=%s", absDstPath, absDirpath)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":     err == nil,
			"description": message,
		})
		return
	}

	debugLog("上传完成，返回成功响应: 目标=%s", dstPath)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"destination": dstPath,
	})
}

type FileJSONInfo struct {
	Name    string      `json:"name"`
	Type    string      `json:"type"`
	Size    int64       `json:"size"`
	Path    string      `json:"path"`
	ModTime int64       `json:"mtime"`
	Extra   interface{} `json:"extra,omitempty"`
}

// path should be absolute
func parseApkInfo(path string) (ai *ApkInfo) {
	defer func() {
		if err := recover(); err != nil {
			errorLog("解析APK信息异常: %v", err)
		}
	}()
	apkf, err := apk.OpenFile(path)
	if err != nil {
		debugLog("打开APK文件失败: %s 错误=%v", path, err)
		return
	}
	ai = &ApkInfo{}
	ai.MainActivity, _ = apkf.MainActivity()
	ai.PackageName = apkf.PackageName()
	ai.Version.Code = apkf.Manifest().VersionCode
	ai.Version.Name = apkf.Manifest().VersionName
	debugLog("解析APK信息: 包名=%s 版本=%s 主Activity=%s", ai.PackageName, ai.Version.Name, ai.MainActivity)
	return
}

func (s *HTTPStaticServer) hInfo(w http.ResponseWriter, r *http.Request) {
	path := mux.Vars(r)["path"]
	relPath := s.getRealPath(r)
	absPath := s.absPath(relPath)

	fi, err := os.Stat(relPath)
	if err != nil {
		errorLog("获取文件信息失败: uri=%s 绝对路径=%s 错误=%v", path, absPath, err)
		http.Error(w, err.Error(), 500)
		return
	}
	fji := &FileJSONInfo{
		Name:    fi.Name(),
		Size:    fi.Size(),
		Path:    path,
		ModTime: fi.ModTime().UnixNano() / 1e6,
	}
	ext := filepath.Ext(path)
	switch ext {
	case ".md":
		fji.Type = "markdown"
	case ".apk":
		fji.Type = "apk"
		debugLog("解析APK信息: %s", absPath)
		fji.Extra = parseApkInfo(relPath)
	case "":
		fji.Type = "dir"
	default:
		fji.Type = "text"
	}
	debugLog("返回文件信息: %s 类型=%s 大小=%d 修改时间=%s", absPath, fji.Type, fi.Size(), fi.ModTime().Format("2006-01-02 15:04:05"))
	data, _ := json.Marshal(fji)
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *HTTPStaticServer) hZip(w http.ResponseWriter, r *http.Request) {
	realPath := s.getRealPath(r)
	infoLog("开始打包: 绝对路径=%s", s.absPath(realPath))
	CompressToZip(w, realPath)
	infoLog("打包完成: 绝对路径=%s", s.absPath(realPath))
}

func (s *HTTPStaticServer) hUnzip(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	zipPath, path := vars["zip_path"], vars["path"]
	absZipPath := s.absPath(filepath.Join(s.Root, zipPath))
	debugLog("提取ZIP文件: 压缩包=%s 目标路径=%s", absZipPath, path)
	ctype := mime.TypeByExtension(filepath.Ext(path))
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	err := ExtractFromZip(filepath.Join(s.Root, zipPath), path, w)
	if err != nil {
		errorLog("提取ZIP失败: 压缩包=%s 目标路径=%s 错误=%v", absZipPath, path, err)
		http.Error(w, err.Error(), 500)
		return
	}
	infoLog("提取ZIP完成: 压缩包=%s 目标路径=%s", absZipPath, path)
}

func combineURL(r *http.Request, path string) *url.URL {
	return &url.URL{
		Scheme: r.URL.Scheme,
		Host:   r.Host,
		Path:   path,
	}
}

func (s *HTTPStaticServer) hPlist(w http.ResponseWriter, r *http.Request) {
	path := mux.Vars(r)["path"]
	if filepath.Ext(path) == ".plist" {
		path = path[0:len(path)-6] + ".ipa"
	}

	relPath := s.getRealPath(r)
	absPath := s.absPath(relPath)
	debugLog("生成Plist: uri=%s 绝对路径=%s", path, absPath)
	plinfo, err := parseIPA(relPath)
	if err != nil {
		errorLog("解析IPA失败: uri=%s 绝对路径=%s 错误=%v", path, absPath, err)
		http.Error(w, err.Error(), 500)
		return
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	baseURL := &url.URL{
		Scheme: scheme,
		Host:   r.Host,
	}
	data, err := generateDownloadPlist(baseURL, path, plinfo)
	if err != nil {
		errorLog("生成Plist XML失败: uri=%s 错误=%v", path, err)
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/xml")
	w.Write(data)
}

func (s *HTTPStaticServer) hIpaLink(w http.ResponseWriter, r *http.Request) {
	path := mux.Vars(r)["path"]
	var plistUrl string

	if r.URL.Scheme == "https" {
		plistUrl = combineURL(r, "/-/ipa/plist/"+path).String()
	} else if s.PlistProxy != "" {
		httpPlistLink := "http://" + r.Host + "/-/ipa/plist/" + path
		debugLog("通过代理生成Plist: 原始链接=%s 代理=%s", httpPlistLink, s.PlistProxy)
		url, err := s.genPlistLink(httpPlistLink)
		if err != nil {
			errorLog("生成Plist链接失败: 路径=%s 错误=%v", path, err)
			http.Error(w, err.Error(), 500)
			return
		}
		plistUrl = url
	} else {
		errorLog("无法生成Plist链接: 非HTTPS且未配置Plist代理")
		http.Error(w, "500: 服务器需使用HTTPS或配置有效的Plist代理", 500)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	infoLog("IPA安装页: 路径=%s Plist链接=%s", path, plistUrl)
	renderHTML(w, "assets/ipa-install.html", map[string]string{
		"Name":      filepath.Base(path),
		"PlistLink": plistUrl,
	})
}

func (s *HTTPStaticServer) genPlistLink(httpPlistLink string) (plistUrl string, err error) {
	pp := s.PlistProxy
	if pp == "" {
		pp = defaultPlistProxy
	}
	debugLog("请求Plist代理: URL=%s 代理=%s", httpPlistLink, pp)
	resp, err := http.Get(httpPlistLink)
	if err != nil {
		errorLog("获取Plist文件失败: URL=%s 错误=%v", httpPlistLink, err)
		return
	}
	defer resp.Body.Close()

	data, _ := ioutil.ReadAll(resp.Body)
	retData, err := http.Post(pp, "text/xml", bytes.NewBuffer(data))
	if err != nil {
		errorLog("上传Plist到代理失败: 代理=%s 错误=%v", pp, err)
		return
	}
	defer retData.Body.Close()

	jsonData, _ := ioutil.ReadAll(retData.Body)
	var ret map[string]string
	if err = json.Unmarshal(jsonData, &ret); err != nil {
		errorLog("解析代理响应失败: 响应=%s 错误=%v", string(jsonData), err)
		return
	}
	plistUrl = pp + "/" + ret["key"]
	debugLog("Plist代理返回链接: %s", plistUrl)
	return
}

func (s *HTTPStaticServer) hFileOrDirectory(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, s.getRealPath(r))
}

type HTTPFileInfo struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Type    string `json:"type"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
}

type AccessTable struct {
	Regex string `yaml:"regex"`
	Allow bool   `yaml:"allow"`
}

type UserControl struct {
	Email string
	// Access bool
	Upload bool
	Delete bool
	Token  string
}

type AccessConf struct {
	Upload       bool          `yaml:"upload" json:"upload"`
	Delete       bool          `yaml:"delete" json:"delete"`
	Users        []UserControl `yaml:"users" json:"users"`
	AccessTables []AccessTable `yaml:"accessTables"`
}

var reCache = make(map[string]*regexp.Regexp)

func (c *AccessConf) canAccess(fileName string) bool {
	for _, table := range c.AccessTables {
		pattern, ok := reCache[table.Regex]
		if !ok {
			pattern, _ = regexp.Compile(table.Regex)
			reCache[table.Regex] = pattern
			if pattern != nil {
				debugLog("编译访问控制正则: %s 允许=%v", table.Regex, table.Allow)
			}
		}
		if pattern == nil {
			continue
		}
		if pattern.MatchString(fileName) {
			debugLog("文件名%s匹配访问规则: regex=%s allow=%v", fileName, table.Regex, table.Allow)
			return table.Allow
		}
	}
	return true
}

func (c *AccessConf) canDelete(r *http.Request) bool {
	session, err := store.Get(r, defaultSessionName)
	if err != nil {
		return c.Delete
	}
	val := session.Values["user"]
	if val == nil {
		return c.Delete
	}
	userInfo := val.(*UserInfo)
	for _, rule := range c.Users {
		if rule.Email == userInfo.Email {
			return rule.Delete
		}
	}
	return c.Delete
}

func (c *AccessConf) canUploadByToken(token string) bool {
	for _, rule := range c.Users {
		if rule.Token == token {
			return rule.Upload
		}
	}
	return c.Upload
}

func (c *AccessConf) canUpload(r *http.Request) bool {
	token := r.FormValue("token")
	if token != "" {
		return c.canUploadByToken(token)
	}
	session, err := store.Get(r, defaultSessionName)
	if err != nil {
		return c.Upload
	}
	val := session.Values["user"]
	if val == nil {
		return c.Upload
	}
	userInfo := val.(*UserInfo)

	for _, rule := range c.Users {
		if rule.Email == userInfo.Email {
			return rule.Upload
		}
	}
	return c.Upload
}

func (s *HTTPStaticServer) hJSONList(w http.ResponseWriter, r *http.Request) {
	requestPath := mux.Vars(r)["path"]
	realPath := s.getRealPath(r)
	search := r.FormValue("search")
	absPath := s.absPath(realPath)
	debugLog("列出目录JSON: 请求路径=%s 绝对路径=%s 搜索关键词=%s", requestPath, absPath, search)

	auth := s.readAccessConf(realPath)
	auth.Upload = auth.canUpload(r)
	auth.Delete = auth.canDelete(r)
	debugLog("目录权限: 上传=%v 删除=%v", auth.Upload, auth.Delete)
	maxDepth := s.DeepPathMaxDepth

	fileInfoMap := make(map[string]os.FileInfo, 0)

	if search != "" {
		results := s.findIndex(search)
		if len(results) > 50 {
			results = results[:50]
		}
		debugLog("搜索索引匹配: 关键词=%s 结果数=%d (限制50)", search, len(results))
		for _, item := range results {
			if filepath.HasPrefix(item.Path, requestPath) {
				fileInfoMap[item.Path] = item.Info
			}
		}
	} else {
		infos, err := ioutil.ReadDir(realPath)
		if err != nil {
			errorLog("读取目录内容失败: 绝对路径=%s 错误=%v", absPath, err)
			http.Error(w, err.Error(), 500)
			return
		}
		debugLog("读取目录: 绝对路径=%s 直接子项数=%d", absPath, len(infos))
		for _, info := range infos {
			childPath := filepath.Join(requestPath, info.Name())
			fileInfoMap[childPath] = info
		}
	}

	lrs := make([]HTTPFileInfo, 0)
	skippedCount := 0
	for path, info := range fileInfoMap {
		if !auth.canAccess(info.Name()) {
			debugLog("文件被访问控制规则跳过: %s", info.Name())
			skippedCount++
			continue
		}
		lr := HTTPFileInfo{
			Name:    info.Name(),
			Path:    path,
			ModTime: info.ModTime().UnixNano() / 1e6,
		}
		if search != "" {
			name, err := filepath.Rel(requestPath, path)
			if err != nil {
				warnLog("计算相对路径失败: 请求路径=%s 路径=%s 错误=%v", requestPath, path, err)
			}
			lr.Name = filepath.ToSlash(name)
		}
		if info.IsDir() {
			name := deepPath(realPath, info.Name(), maxDepth)
			lr.Name = name
			lr.Path = filepath.Join(filepath.Dir(path), name)
			lr.Type = "dir"
			lr.Size = s.historyDirSize(lr.Path)
		} else {
			lr.Type = "file"
			lr.Size = info.Size()
		}
		lrs = append(lrs, lr)
	}

	debugLog("目录列表返回: 路径=%s 文件数=%d 跳过数=%d", absPath, len(lrs), skippedCount)
	data, _ := json.Marshal(map[string]interface{}{
		"files": lrs,
		"auth":  auth,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

var dirInfoSize = Directory{size: make(map[string]int64), mutex: &sync.RWMutex{}}

func (s *HTTPStaticServer) makeIndex() error {
	var indexes = make([]IndexFileItem, 0)
	startTime := time.Now()
	var errCount int
	err := filepath.Walk(s.Root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			warnLog("遍历路径出错: %s 错误=%v", strconv.Quote(path), err)
			errCount++
			return filepath.SkipDir
		}
		if info.IsDir() {
			return nil
		}

		path, _ = filepath.Rel(s.Root, path)
		path = filepath.ToSlash(path)
		indexes = append(indexes, IndexFileItem{path, info})
		return nil
	})
	s.indexes = indexes
	debugLog("搜索索引构建完成: 文件数=%d 遍历错误数=%d 耗时=%v", len(indexes), errCount, time.Since(startTime))
	return err
}

func (s *HTTPStaticServer) historyDirSize(dir string) int64 {
	dirInfoSize.mutex.RLock()
	size, ok := dirInfoSize.size[dir]
	dirInfoSize.mutex.RUnlock()

	if ok {
		return size
	}

	for _, fitem := range s.indexes {
		if filepath.HasPrefix(fitem.Path, dir) {
			size += fitem.Info.Size()
		}
	}

	dirInfoSize.mutex.Lock()
	dirInfoSize.size[dir] = size
	dirInfoSize.mutex.Unlock()

	return size
}

func (s *HTTPStaticServer) findIndex(text string) []IndexFileItem {
	ret := make([]IndexFileItem, 0)
	for _, item := range s.indexes {
		ok := true
		// search algorithm, space for AND
		for _, keyword := range strings.Fields(text) {
			needContains := true
			if strings.HasPrefix(keyword, "-") {
				needContains = false
				keyword = keyword[1:]
			}
			if keyword == "" {
				continue
			}
			ok = (needContains == strings.Contains(strings.ToLower(item.Path), strings.ToLower(keyword)))
			if !ok {
				break
			}
		}
		if ok {
			ret = append(ret, item)
		}
	}
	return ret
}

func (s *HTTPStaticServer) defaultAccessConf() AccessConf {
	return AccessConf{
		Upload: s.Upload,
		Delete: s.Delete,
	}
}

func (s *HTTPStaticServer) readAccessConf(realPath string) (ac AccessConf) {
	absPath := s.absPath(realPath)
	debugLog("读取访问控制配置: 绝对路径=%s", absPath)
	relativePath, err := filepath.Rel(s.Root, realPath)
	if err != nil || relativePath == "." || relativePath == "" {
		debugLog("路径在根目录或计算失败，使用默认配置: realPath=%s root=%s err=%v", realPath, s.Root, err)
		ac = s.defaultAccessConf()
		debugLog("默认权限: 上传=%v 删除=%v", ac.Upload, ac.Delete)
		realPath = s.Root
	} else {
		parentPath := filepath.Dir(realPath)
		debugLog("递归读取父目录配置: 当前=%s 父目录=%s", realPath, parentPath)
		ac = s.readAccessConf(parentPath)
	}
	if isFile(realPath) {
		debugLog("当前路径是文件，取其所在目录: %s", filepath.Dir(realPath))
		realPath = filepath.Dir(realPath)
	}
	cfgFile := filepath.Join(realPath, YAMLCONF)
	data, err := ioutil.ReadFile(cfgFile)
	if err != nil {
		if os.IsNotExist(err) {
			debugLog(".ghs.yml不存在: %s，沿用上级配置(上传=%v 删除=%v)", s.absPath(cfgFile), ac.Upload, ac.Delete)
			return
		}
		warnLog("读取.ghs.yml出错: %s 错误=%v", s.absPath(cfgFile), err)
	} else {
		debugLog("读取到.ghs.yml配置: %s 大小=%d字节 内容=%s", s.absPath(cfgFile), len(data), string(data))
	}
	err = yaml.Unmarshal(data, &ac)
	if err != nil {
		warnLog("解析.ghs.yml出错: %s 错误=%v", s.absPath(cfgFile), err)
	} else {
		debugLog(".ghs.yml解析成功: 上传=%v 删除=%v 用户数=%d 访问规则数=%d",
			ac.Upload, ac.Delete, len(ac.Users), len(ac.AccessTables))
	}
	return
}

func deepPath(basedir, name string, maxDepth int) string {
	for depth := 0; depth <= maxDepth; depth += 1 {
		finfos, err := ioutil.ReadDir(filepath.Join(basedir, name))
		if err != nil {
			debugLog("deepPath读取目录失败: basedir=%s name=%s 深度=%d 错误=%v", basedir, name, depth, err)
			break
		}
		if len(finfos) != 1 {
			debugLog("deepPath终止: 目录%s下有%d个子项(深度=%d)", filepath.Join(basedir, name), len(finfos), depth)
			break
		}
		if finfos[0].IsDir() {
			oldName := name
			name = filepath.ToSlash(filepath.Join(name, finfos[0].Name()))
			debugLog("deepPath继续深入: %s -> %s (深度=%d)", oldName, name, depth+1)
		} else {
			debugLog("deepPath终止: 唯一子项为文件 %s (深度=%d)", finfos[0].Name(), depth)
			break
		}
	}
	return name
}

func assetsContent(name string) string {
	fd, err := Assets.Open(name)
	if err != nil {
		errorLog("打开资源文件失败: %s 错误=%v", name, err)
		panic(err)
	}
	data, err := ioutil.ReadAll(fd)
	if err != nil {
		errorLog("读取资源文件失败: %s 错误=%v", name, err)
		panic(err)
	}
	debugLog("加载资源文件: %s 大小=%d字节", name, len(data))
	return string(data)
}

// TODO: I need to read more abouthtml/template
var (
	funcMap template.FuncMap
)

func init() {
	funcMap = template.FuncMap{
		"title": strings.Title,
		"urlhash": func(path string) string {
			httpFile, err := Assets.Open("assets/" + path)
			if err != nil {
				return path + "#no-such-file"
			}
			info, err := httpFile.Stat()
			if err != nil {
				return path + "#stat-error"
			}
			return fmt.Sprintf("%s?t=%d", path, info.ModTime().Unix())
		},
	}
}

var (
	_tmpls = make(map[string]*template.Template)
)

func renderHTML(w http.ResponseWriter, name string, v interface{}) {
	if t, ok := _tmpls[name]; ok {
		debugLog("使用缓存模板: %s", name)
		t.Execute(w, v)
		return
	}
	debugLog("加载并解析模板: %s", name)
	t := template.Must(template.New(name).Funcs(funcMap).Delims("[[", "]]").Parse(assetsContent(name)))
	_tmpls[name] = t
	if err := t.Execute(w, v); err != nil {
		errorLog("模板执行失败: %s 错误=%v", name, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	debugLog("模板渲染完成: %s", name)
}

func checkFilename(name string) error {
	if strings.ContainsAny(name, "\\/:*<>|") {
		return errors.New("文件名不能包含 \\/:*<>| 等特殊字符")
	}
	return nil
}

func (s *HTTPStaticServer) hVideoPlayer(w http.ResponseWriter, r *http.Request) {
	path := mux.Vars(r)["path"]
	realPath := s.getRealPath(r)
	absPath := s.absPath(realPath)
	extension := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))

	if _, err := os.Stat(realPath); os.IsNotExist(err) {
		errorLog("视频文件不存在: uri=%s 绝对路径=%s", path, absPath)
		http.Error(w, "文件未找到", http.StatusNotFound)
		return
	}

	fileName := filepath.Base(path)

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	videoURL := fmt.Sprintf("%s://%s/%s", scheme, r.Host, path)
	debugLog("视频播放器: 文件=%s URL=%s 扩展名=%s", absPath, videoURL, extension)

	renderHTML(w, "assets/video-player.html", map[string]interface{}{
		"FileName":  fileName,
		"VideoURL":  videoURL,
		"Extension": extension,
	})
}