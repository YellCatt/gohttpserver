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

const contentSecurityPolicy = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; media-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'self'; object-src 'none'; frame-ancestors 'none'"

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

	// routers for Apple *.ipa
	m.HandleFunc("/-/ipa/plist/{path:.*}", s.hPlist)
	m.HandleFunc("/-/ipa/link/{path:.*}", s.hIpaLink)
	m.HandleFunc("/-/video-player/{path:.*}", s.hVideoPlayer)

	m.HandleFunc("/{path:.*}", s.hIndex).Methods("GET", "HEAD")
	m.HandleFunc("/{path:.*}", s.hUploadOrMkdir).Methods("POST")
	m.HandleFunc("/{path:.*}", s.hDelete).Methods("DELETE")
	return s
}

func (s *HTTPStaticServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Defense-in-depth for uploaded content and README previews.
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	s.m.ServeHTTP(w, r)
}

// Return real path with Seperator(/)
func (s *HTTPStaticServer) getRealPath(r *http.Request) string {
	path := mux.Vars(r)["path"]
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	path = filepath.Clean(path) // prevent .. for safe issues
	relativePath, err := filepath.Rel(s.Prefix, path)
	if err != nil {
		relativePath = path
	}
	realPath := filepath.Join(s.Root, relativePath)
	return filepath.ToSlash(realPath)
}

func (s *HTTPStaticServer) absPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(abs)
}

func (s *HTTPStaticServer) hIndex(w http.ResponseWriter, r *http.Request) {
	path := mux.Vars(r)["path"]
	realPath := s.getRealPath(r)
	if r.FormValue("json") == "true" {
		debugLog("列出目录: uri=%s 绝对路径=%s", path, s.absPath(realPath))
		s.hJSONList(w, r)
		return
	}

	if r.FormValue("op") == "info" {
		debugLog("查询文件信息: uri=%s 绝对路径=%s", path, s.absPath(realPath))
		s.hInfo(w, r)
		return
	}

	if r.FormValue("op") == "archive" {
		debugLog("打包下载: uri=%s 绝对路径=%s", path, s.absPath(realPath))
		s.hZip(w, r)
		return
	}

	debugLog("请求: 方法=%s URI=%s 绝对路径=%s 是否目录=%v", r.Method, path, s.absPath(realPath), isDir(realPath))
	if r.FormValue("raw") == "false" || isDir(realPath) {
		if r.Method == "HEAD" {
			debugLog("HEAD请求，直接返回")
			return
		}
		debugLog("返回目录列表页面")
		renderHTML(w, "assets/index.html", s)
	} else {
		if filepath.Base(path) == YAMLCONF {
			debugLog("检测到访问.ghs.yml配置文件，进行权限检查")
			auth := s.readAccessConf(realPath)
			if !auth.Delete {
				warnLog("尝试访问受限配置文件被拒绝: %s", s.absPath(realPath))
				http.Error(w, "安全警告，禁止读取此文件", http.StatusForbidden)
				return
			}
		}
		if r.FormValue("download") == "true" {
			debugLog("下载文件: %s", s.absPath(realPath))
			w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(filepath.Base(path)))
		} else {
			debugLog("返回文件内容: %s", s.absPath(realPath))
		}
		http.ServeFile(w, r, realPath)
	}
}

func (s *HTTPStaticServer) hDelete(w http.ResponseWriter, req *http.Request) {
	path := mux.Vars(req)["path"]
	realPath := s.getRealPath(req)
	absPath := s.absPath(realPath)
	auth := s.readAccessConf(realPath)
	if !auth.canDelete(req) {
		warnLog("删除被拒绝: uri=%s 绝对路径=%s", path, absPath)
		http.Error(w, "禁止删除", http.StatusForbidden)
		return
	}

	infoLog("开始删除: uri=%s 绝对路径=%s", path, absPath)
	err := os.RemoveAll(realPath)
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

func (s *HTTPStaticServer) hUploadOrMkdir(w http.ResponseWriter, req *http.Request) {
	dirpath := s.getRealPath(req)
	absDirpath := s.absPath(dirpath)
	debugLog("上传/建目录请求: 方法=%s 目录=%s 绝对路径=%s", req.Method, dirpath, absDirpath)

	auth := s.readAccessConf(dirpath)
	if !auth.canUpload(req) {
		warnLog("上传被拒绝: 绝对路径=%s", absDirpath)
		http.Error(w, "禁止上传", http.StatusForbidden)
		return
	}

	file, header, err := req.FormFile("file")

	dirExisted := true
	if _, err := os.Stat(dirpath); os.IsNotExist(err) {
		dirExisted = false
		if err := os.MkdirAll(dirpath, os.ModePerm); err != nil {
			errorLog("创建目录失败: 绝对路径=%s 错误=%v", absDirpath, err)
			http.Error(w, "目录创建失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		infoLog("创建目录: 绝对路径=%s", absDirpath)
	}

	if file == nil {
		if dirExisted {
			infoLog("目录已存在，无需创建: 绝对路径=%s", absDirpath)
		}
		w.Header().Set("Content-Type", "application/json;charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":     true,
			"destination": dirpath,
		})
		return
	}

	if err != nil {
		errorLog("解析上传表单失败: 绝对路径=%s 错误=%v", absDirpath, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() {
		file.Close()
		req.MultipartForm.RemoveAll()
	}()

	filename := req.FormValue("filename")
	if filename == "" {
		filename = header.Filename
	}
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
		infoLog("开始修改文件: 文件名=%s 大小=%d 绝对路径=%s", filename, header.Size, absDstPath)
	} else {
		infoLog("开始创建文件: 文件名=%s 大小=%d 绝对路径=%s", filename, header.Size, absDstPath)
	}

	var copyErr error
	dst, err := os.Create(dstPath)
	if err != nil {
		errorLog("创建文件失败: 绝对路径=%s 错误=%v", absDstPath, err)
		http.Error(w, "文件创建失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	buf := s.bufPool.Get().([]byte)
	defer s.bufPool.Put(buf)
	_, copyErr = io.CopyBuffer(dst, file, buf)
	dst.Close()
	if copyErr != nil {
		errorLog("写入文件失败: 绝对路径=%s 错误=%v", absDstPath, copyErr)
		http.Error(w, copyErr.Error(), http.StatusInternalServerError)
		return
	}

	infoLog("文件操作完成: 文件名=%s 大小=%d 绝对路径=%s", filename, header.Size, absDstPath)

	w.Header().Set("Content-Type", "application/json;charset=utf-8")

	if req.FormValue("unzip") == "true" {
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
		fji.Extra = parseApkInfo(relPath)
	case "":
		fji.Type = "dir"
	default:
		fji.Type = "text"
	}
	debugLog("返回文件信息: %s 类型=%s 大小=%d", absPath, fji.Type, fi.Size())
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
		}
		// skip wrong format regex
		if pattern == nil {
			continue
		}
		if pattern.MatchString(fileName) {
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
		debugLog("搜索索引匹配: 关键词=%s 结果数=%d", search, len(results))
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
		for _, info := range infos {
			fileInfoMap[filepath.Join(requestPath, info.Name())] = info
		}
	}

	lrs := make([]HTTPFileInfo, 0)
	for path, info := range fileInfoMap {
		if !auth.canAccess(info.Name()) {
			debugLog("文件被访问控制规则跳过: %s", info.Name())
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

	debugLog("目录列表返回: 路径=%s 文件数=%d", absPath, len(lrs))
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
	var err = filepath.Walk(s.Root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			warnLog("遍历路径出错: %s 错误=%v", strconv.Quote(path), err)
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
	debugLog("搜索索引构建完成: 文件数=%d", len(indexes))
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
	debugLog("读取访问控制配置: 绝对路径=%s", s.absPath(realPath))
	relativePath, err := filepath.Rel(s.Root, realPath)
	if err != nil || relativePath == "." || relativePath == "" {
		ac = s.defaultAccessConf()
		realPath = s.Root
	} else {
		parentPath := filepath.Dir(realPath)
		ac = s.readAccessConf(parentPath)
	}
	if isFile(realPath) {
		realPath = filepath.Dir(realPath)
	}
	cfgFile := filepath.Join(realPath, YAMLCONF)
	data, err := ioutil.ReadFile(cfgFile)
	if err != nil {
		if os.IsNotExist(err) {
			debugLog(".ghs.yml不存在，使用上级配置: %s", s.absPath(cfgFile))
			return
		}
		warnLog("读取.ghs.yml出错: %s 错误=%v", s.absPath(cfgFile), err)
	} else {
		debugLog("读取到.ghs.yml配置: %s 大小=%d字节", s.absPath(cfgFile), len(data))
	}
	err = yaml.Unmarshal(data, &ac)
	if err != nil {
		warnLog("解析.ghs.yml出错: %s 错误=%v", s.absPath(cfgFile), err)
	}
	return
}

func deepPath(basedir, name string, maxDepth int) string {
	// loop max 5, incase of for loop not finished
	for depth := 0; depth <= maxDepth; depth += 1 {
		finfos, err := ioutil.ReadDir(filepath.Join(basedir, name))
		if err != nil || len(finfos) != 1 {
			break
		}
		if finfos[0].IsDir() {
			name = filepath.ToSlash(filepath.Join(name, finfos[0].Name()))
		} else {
			break
		}
	}
	return name
}

func assetsContent(name string) string {
	fd, err := Assets.Open(name)
	if err != nil {
		panic(err)
	}
	data, err := ioutil.ReadAll(fd)
	if err != nil {
		panic(err)
	}
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
			httpFile, err := Assets.Open(path)
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
		t.Execute(w, v)
		return
	}
	t := template.Must(template.New(name).Funcs(funcMap).Delims("[[", "]]").Parse(assetsContent(name)))
	_tmpls[name] = t
	t.Execute(w, v)
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