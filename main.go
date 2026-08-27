package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/alecthomas/kingpin"
	accesslog "github.com/codeskyblue/go-accesslog"
	"github.com/go-yaml/yaml"
	"github.com/goji/httpauth"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
)

type Configure struct {
	Conf            *os.File `yaml:"-"`
	Addr            string   `yaml:"addr"`
	Port            int      `yaml:"port"`
	Root            string   `yaml:"root"`
	Prefix          string   `yaml:"prefix"`
	HTTPAuth        string   `yaml:"httpauth"`
	Cert            string   `yaml:"cert"`
	Key             string   `yaml:"key"`
	Theme           string   `yaml:"theme"`
	XHeaders        bool     `yaml:"xheaders"`
	Upload          bool     `yaml:"upload"`
	Delete          bool     `yaml:"delete"`
	PlistProxy      string   `yaml:"plistproxy"`
	Title           string   `yaml:"title"`
	Debug           bool     `yaml:"debug"`
	LogFile         string   `yaml:"log-file"`
	GoogleTrackerID string   `yaml:"google-tracker-id"`
	Auth            struct {
		Type   string            `yaml:"type"` // openid|http|github
		OpenID string            `yaml:"openid"`
		HTTP   []string          `yaml:"http"`
		Users  map[string]string `yaml:"users"`
		ID     string            `yaml:"id"`     // for oauth2
		Secret string            `yaml:"secret"` // for oauth2
	} `yaml:"auth"`
	DeepPathMaxDepth int  `yaml:"deep-path-max-depth"`
	NoIndex          bool `yaml:"no-index"`
}

type httpLogger struct{}

func (l httpLogger) Log(record accesslog.LogRecord) {
	log.Printf("%s - %s %d %s", record.Ip, record.Method, record.Status, record.Uri)
}

func debugLog(format string, args ...interface{}) {
	if gcfg.Debug {
		log.Printf("[DEBUG] "+format, args...)
	}
}

func infoLog(format string, args ...interface{}) {
	log.Printf("[INFO] "+format, args...)
}

func warnLog(format string, args ...interface{}) {
	log.Printf("[WARN] "+format, args...)
}

func errorLog(format string, args ...interface{}) {
	log.Printf("[ERROR] "+format, args...)
}

var (
	defaultPlistProxy = "https://plistproxy.herokuapp.com/plist"
	defaultOpenID     = "https://login.netease.com/openid"
	gcfg              = Configure{}
	logger            = httpLogger{}
	confPath          string

	VERSION   = "unknown"
	BUILDTIME = "unknown time"
	GITCOMMIT = "unknown git commit"
	SITE      = "https://github.com/codeskyblue/gohttpserver"
)

func versionMessage() string {
	t := template.Must(template.New("version").Parse(`GoHTTPServer
  Version:        {{.Version}}
  Go version:     {{.GoVersion}}
  OS/Arch:        {{.OSArch}}
  Git commit:     {{.GitCommit}}
  Built:          {{.Built}}
  Site:           {{.Site}}`))
	buf := bytes.NewBuffer(nil)
	t.Execute(buf, map[string]interface{}{
		"Version":   VERSION,
		"GoVersion": runtime.Version(),
		"OSArch":    runtime.GOOS + "/" + runtime.GOARCH,
		"GitCommit": GITCOMMIT,
		"Built":     BUILDTIME,
		"Site":      SITE,
	})
	return buf.String()
}

func parseFlags() error {
	gcfg.Root = "./files"
	gcfg.Port = 9100
	gcfg.Addr = ""
	gcfg.Theme = "black"
	gcfg.PlistProxy = defaultPlistProxy
	gcfg.Auth.OpenID = defaultOpenID
	gcfg.GoogleTrackerID = "UA-81205425-2"
	gcfg.Title = "Go HTTP File Server"
	gcfg.DeepPathMaxDepth = 5
	gcfg.NoIndex = false
	gcfg.LogFile = "gohttpserver.log"

	defaultCfg := gcfg
	defaultCfg.Auth.Type = "http"
	defaultCfg.Auth.Users = map[string]string{"admin": "asd123456"}

	kingpin.HelpFlag.Short('h')
	kingpin.Version(versionMessage())
	kingpin.Flag("conf", "config file path, yaml format").StringVar(&confPath)
	kingpin.Flag("root", "root directory, default ./").Short('r').StringVar(&gcfg.Root)
	kingpin.Flag("prefix", "url prefix, eg /foo").StringVar(&gcfg.Prefix)
	kingpin.Flag("port", "listen port, default 9100").IntVar(&gcfg.Port)
	kingpin.Flag("addr", "listen address, eg 127.0.0.1:9100").Short('a').StringVar(&gcfg.Addr)
	kingpin.Flag("cert", "tls cert.pem path").StringVar(&gcfg.Cert)
	kingpin.Flag("key", "tls key.pem path").StringVar(&gcfg.Key)
	kingpin.Flag("auth-type", "Auth type <http|openid>").StringVar(&gcfg.Auth.Type)
	kingpin.Flag("auth-http", "HTTP basic auth (ex: user:pass)").StringsVar(&gcfg.Auth.HTTP)
	kingpin.Flag("auth-openid", "OpenID auth identity url").StringVar(&gcfg.Auth.OpenID)
	kingpin.Flag("theme", "web theme, one of <black|green>").StringVar(&gcfg.Theme)
	kingpin.Flag("upload", "enable upload support").BoolVar(&gcfg.Upload)
	kingpin.Flag("delete", "enable delete support").BoolVar(&gcfg.Delete)
	kingpin.Flag("xheaders", "used when behide nginx").BoolVar(&gcfg.XHeaders)
	kingpin.Flag("debug", "enable debug mode").BoolVar(&gcfg.Debug)
	kingpin.Flag("plistproxy", "plist proxy when server is not https").Short('p').StringVar(&gcfg.PlistProxy)
	kingpin.Flag("title", "server title").StringVar(&gcfg.Title)
	kingpin.Flag("google-tracker-id", "set to empty to disable it").StringVar(&gcfg.GoogleTrackerID)
	kingpin.Flag("deep-path-max-depth", "set to -1 to not combine dirs").IntVar(&gcfg.DeepPathMaxDepth)
	kingpin.Flag("no-index", "disable indexing").BoolVar(&gcfg.NoIndex)
	kingpin.Flag("log-file", "log file path, default gohttpserver.log, set empty to disable").StringVar(&gcfg.LogFile)

	kingpin.Parse()

	if confPath == "" {
		confPath = "config.yaml"
	}

	debugLog("使用配置文件路径: %s", confPath)

	if _, err := os.Stat(confPath); os.IsNotExist(err) {
		debugLog("配置文件不存在，创建默认配置: %s", confPath)
		data, err := yaml.Marshal(defaultCfg)
		if err != nil {
			errorLog("序列化默认配置失败: %v", err)
			return err
		}
		if err := ioutil.WriteFile(confPath, data, 0644); err != nil {
			errorLog("写入默认配置文件失败: %v", err)
			return err
		}
		infoLog("已创建默认配置文件: %s", confPath)
	}

	ymlData, err := ioutil.ReadFile(confPath)
	if err != nil {
		errorLog("读取配置文件失败: %v", err)
		return err
	}
	debugLog("读取配置文件成功，大小: %d 字节", len(ymlData))

	if err := yaml.Unmarshal(ymlData, &gcfg); err != nil {
		errorLog("解析配置文件失败: %v", err)
		return err
	}
	infoLog("配置文件加载成功")

	if gcfg.Auth.Type == "http" && len(gcfg.Auth.Users) == 0 && len(gcfg.Auth.HTTP) == 0 {
		gcfg.Auth.Users = map[string]string{"admin": "asd123456"}
		debugLog("未配置HTTP用户，使用默认账户")
	}

	kingpin.Parse()
	debugLog("命令行参数解析完成")

	return nil
}

func fixPrefix(prefix string) string {
	prefix = regexp.MustCompile(`/*$`).ReplaceAllString(prefix, "")
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	if prefix == "/" {
		prefix = ""
	}
	return prefix
}

func cors(next http.Handler) http.Handler {
	// access control and CORS middleware
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == "OPTIONS" {
			return
		}
		next.ServeHTTP(w, r)
	})
}

func multiBasicAuth(userPassMap map[string]string) func(http.Handler) http.Handler {
	return httpauth.BasicAuth(httpauth.AuthOptions{
		Realm: "Restricted",
		AuthFunc: func(user, pass string, request *http.Request) bool {
			password, ok := userPassMap[user]
			if !ok {
				return false
			}
			givenPass := sha256.Sum256([]byte(pass))
			requiredPass := sha256.Sum256([]byte(password))
			return subtle.ConstantTimeCompare(givenPass[:], requiredPass[:]) == 1
		},
	})
}

func main() {
	if err := parseFlags(); err != nil {
		log.Fatal(err)
	}

	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	time.Local = loc
	infoLog("时区设置: %s", loc)

	if gcfg.Debug {
		data, _ := yaml.Marshal(gcfg)
		fmt.Printf("--- 配置信息 ---\n%s\n", string(data))
		debugLog("调试模式已开启")
	}
	log.SetFlags(log.Lshortfile | log.LstdFlags)
	debugLog("日志格式: 文件位置 + 标准时间戳")

	if gcfg.LogFile != "" {
		logFile, err := os.OpenFile(gcfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			errorLog("打开日志文件失败: %v", err)
			log.Fatal(err)
		}
		log.SetOutput(io.MultiWriter(os.Stderr, logFile))
		infoLog("日志文件: %s", gcfg.LogFile)
	} else {
		infoLog("未配置日志文件，日志仅输出到控制台")
	}

	gcfg.Prefix = fixPrefix(gcfg.Prefix)
	if gcfg.Prefix != "" {
		infoLog("URL前缀: %s", gcfg.Prefix)
	}

	if err := os.MkdirAll(gcfg.Root, os.ModePerm); err != nil {
		errorLog("创建根目录失败: %v", err)
		log.Fatal("创建根目录失败:", err)
	}

	ss := NewHTTPStaticServer(gcfg.Root, gcfg.NoIndex)
	ss.Prefix = gcfg.Prefix
	ss.Theme = gcfg.Theme
	ss.Title = gcfg.Title
	ss.GoogleTrackerID = gcfg.GoogleTrackerID
	ss.Upload = gcfg.Upload
	ss.Delete = gcfg.Delete
	ss.AuthType = gcfg.Auth.Type
	ss.DeepPathMaxDepth = gcfg.DeepPathMaxDepth

	if gcfg.PlistProxy != "" {
		u, err := url.Parse(gcfg.PlistProxy)
		if err != nil {
			errorLog("解析PlistProxy地址失败: %v", err)
			log.Fatal(err)
		}
		u.Scheme = "https"
		ss.PlistProxy = u.String()
	}
	if ss.PlistProxy != "" {
		infoLog("Plist代理地址: %s", strconv.Quote(ss.PlistProxy))
	}

	var hdlr http.Handler = ss
	hdlr = accesslog.NewLoggingHandler(hdlr, logger)

	switch gcfg.Auth.Type {
	case "http":
		userPassMap := make(map[string]string)
		for _, auth := range gcfg.Auth.HTTP {
			userpass := strings.SplitN(auth, ":", 2)
			if len(userpass) == 2 {
				userPassMap[userpass[0]] = userpass[1]
			}
		}
		for user, pass := range gcfg.Auth.Users {
			userPassMap[user] = pass
		}
		if len(userPassMap) > 0 {
			hdlr = multiBasicAuth(userPassMap)(hdlr)
			infoLog("HTTP基本认证已启用，用户数: %d", len(userPassMap))
		} else {
			debugLog("未配置HTTP认证用户")
		}
	case "openid":
		handleOpenID(gcfg.Auth.OpenID, false)
		infoLog("OpenID认证已启用: %s", gcfg.Auth.OpenID)
	case "oauth2-proxy":
		handleOauth2()
		infoLog("OAuth2代理认证已启用")
	default:
		infoLog("未启用认证 (auth-type=%s)", gcfg.Auth.Type)
	}

	hdlr = cors(hdlr)

	if gcfg.XHeaders {
		hdlr = handlers.ProxyHeaders(hdlr)
		infoLog("启用X-Headers支持 (用于Nginx反向代理)")
	}

	mainRouter := mux.NewRouter()
	router := mainRouter
	if gcfg.Prefix != "" {
		router = mainRouter.PathPrefix(gcfg.Prefix).Subrouter()
		mainRouter.Handle(gcfg.Prefix, hdlr)
		mainRouter.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, gcfg.Prefix, http.StatusTemporaryRedirect)
		})
		debugLog("路由前缀已配置: %s", gcfg.Prefix)
	}

	router.PathPrefix("/-/assets/").Handler(http.StripPrefix(gcfg.Prefix+"/-/", http.FileServer(Assets)))
	router.HandleFunc("/-/sysinfo", func(w http.ResponseWriter, r *http.Request) {
		data, _ := json.Marshal(map[string]interface{}{
			"version": VERSION,
		})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.Write(data)
	})
	router.PathPrefix("/").Handler(hdlr)

	if gcfg.Addr == "" {
		gcfg.Addr = fmt.Sprintf(":%d", gcfg.Port)
	}
	if !strings.Contains(gcfg.Addr, ":") {
		gcfg.Addr = ":" + gcfg.Addr
	}
	host, port, _ := net.SplitHostPort(gcfg.Addr)
	if host == "" {
		host = "0.0.0.0"
	}
	scheme := "http"
	if gcfg.Key != "" && gcfg.Cert != "" {
		scheme = "https"
	}
	localIP := getLocalIP()
	fmt.Println("========================================")
	fmt.Printf("  Go HTTP File Server v%s\n", VERSION)
	fmt.Println("========================================")
	fmt.Printf("  监听地址:    %s://%s:%s\n", scheme, host, port)
	fmt.Printf("  本机访问:    %s://localhost:%s\n", scheme, port)
	if localIP != "" {
		fmt.Printf("  网络访问:    %s://%s:%s\n", scheme, localIP, port)
	}
	absRoot, _ := filepath.Abs(gcfg.Root)
	fmt.Printf("  根目录:      %s\n", gcfg.Root)
	fmt.Printf("  根目录(绝对): %s\n", filepath.ToSlash(absRoot))
	if gcfg.LogFile != "" {
		fmt.Printf("  日志文件:    %s\n", gcfg.LogFile)
	}
	fmt.Println("========================================")

	srv := &http.Server{
		Handler: mainRouter,
		Addr:    gcfg.Addr,
	}

	err = nil
	if gcfg.Key != "" && gcfg.Cert != "" {
		infoLog("启动HTTPS服务，证书: %s 密钥: %s", gcfg.Cert, gcfg.Key)
		err = srv.ListenAndServeTLS(gcfg.Cert, gcfg.Key)
	} else {
		infoLog("启动HTTP服务，监听: %s", gcfg.Addr)
		err = srv.ListenAndServe()
	}
	if err != nil {
		errorLog("服务启动失败: %v", err)
		log.Fatal(err)
	}
}