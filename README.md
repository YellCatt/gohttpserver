# Go HTTP File Server

一个基于 Go 语言的轻量级 HTTP 文件服务器，单文件二进制，零依赖，开箱即用。

## 功能特性

- HTTP 文件浏览与下载，支持目录列表、面包屑导航
- 文件上传（支持大文件，带实时进度日志）
- 文件删除与新建目录
- ZIP 打包下载 / 上传 ZIP 自动解压
- APK / IPA 安装包识别与二维码分发
- HTTP Basic Auth / OpenID / OAuth2-Proxy 多种认证方式
- 基于 `.ghs.yml` 的目录级权限控制（类似 `.htaccess`）
- 全局文件搜索
- CORS 跨域支持
- Nginx 反向代理友好
- 响应式前端，支持移动端

## 快速开始

```bash
gohttpserver
```

首次启动会自动在当前目录生成 `config.yaml` 配置文件，所有配置均通过此文件管理。

## 配置文件

所有参数通过 `config.yaml` 管理，首次启动自动生成默认配置：

```yaml
addr: ""
port: 9100
root: "./files"
prefix: ""
upload: false
delete: false
theme: "black"
title: "Go HTTP File Server"
debug: false
log-file: "gohttpserver.log"
deep-path-max-depth: 5
no-index: false
plistproxy: "https://plistproxy.herokuapp.com/plist"
auth:
  type: "http"
  users:
    admin: "asd123456"
  http: []
  openid: "https://login.netease.com/openid"
```

### 配置项说明

| 字段 | 说明 | 默认值 |
|------|------|--------|
| `addr` | 监听地址，空为全部接口 | 空 |
| `port` | 监听端口 | `9100` |
| `root` | 文件根目录 | `./files` |
| `prefix` | URL 前缀，如 `/foo` | 空 |
| `upload` | 启用上传功能 | `false` |
| `delete` | 启用删除和新建目录 | `false` |
| `auth.type` | 认证类型：`http` / `openid` / `oauth2-proxy` | `http` |
| `auth.users` | HTTP 认证用户表（用户名:密码） | admin |
| `auth.openid` | OpenID 认证地址 | - |
| `theme` | 主题：`black` / `green` | `black` |
| `debug` | 调试模式，输出详细日志 | `false` |
| `log-file` | 日志文件路径，空则不写文件 | `gohttpserver.log` |
| `xheaders` |  Behind Nginx 时设为 `true` | `false` |
| `plistproxy` | IPA Plist 代理地址 | 默认代理 |
| `deep-path-max-depth` | 目录合并深度，`-1` 禁用 | `5` |
| `no-index` | 禁用文件搜索索引 | `false` |

## 权限控制

在任意目录下创建 `.ghs.yml` 文件，可以精确控制该目录的访问权限：

```yaml
upload: false
delete: false
users:
  - email: "admin@example.com"
    upload: true
    delete: true
    token: "your-secret-token"
accessTables:
  - regex: "\.secret$"
    allow: false
```

- `upload` / `delete`: 控制该目录的上传/删除权限
- `users`: 针对特定用户设置不同权限
- `token`: 用于 API 上传时的身份验证
- `accessTables`: 文件级别的访问控制规则（正则匹配）

## 上传接口

### Web 界面

直接通过浏览器访问，点击上传按钮即可。

### CURL 命令

```bash
# 上传文件
curl -F file=@localfile.txt http://server:port/dir

# 上传并重命名
curl -F file=@localfile.txt -F filename=newname.txt http://server:port/dir

# 使用 token 上传
curl -F file=@file.zip -F token=your-token http://server:port/dir

# 上传并自动解压
curl -F file=@archive.zip -F unzip=true http://server:port/dir
```

注意：文件名不能包含 `\/:*<>|` 字符。

## 认证方式

### HTTP Basic Auth

编辑 `config.yaml`：

```yaml
auth:
  type: "http"
  users:
    admin: "your_password"
    user2: "password2"
```

### OpenID 认证

```yaml
auth:
  type: "openid"
  openid: "https://login.example.com/openid"
```

### OAuth2 Proxy

```yaml
auth:
  type: "oauth2-proxy"
```

由反向代理（如 OAuth2 Proxy）处理认证，后端从请求头读取用户信息：
- `X-Auth-Request-Email`: 用户邮箱
- `X-Auth-Request-Fullname`: 用户昵称
- `X-Auth-Request-User`: 用户名

## 部署

### 编译

```bash
# 当前平台编译
go build -o gohttpserver .

# 交叉编译 (Linux ARM 64位)
GOOS=linux GOARCH=arm64 go build -o gohttpserver .

# 交叉编译 (Linux ARM 32位)
GOOS=linux GOARCH=arm GOARM=7 go build -o gohttpserver .

# 交叉编译 (Linux x86_64)
GOOS=linux GOARCH=amd64 go build -o gohttpserver .
```

### 部署到设备

```bash
# 传输到目标设备
scp gohttpserver user@device:/path/to/

# 在设备上赋予执行权限并运行
chmod +x gohttpserver
./gohttpserver
```

### Nginx 反向代理

```nginx
server {
  listen 80;
  server_name your-domain.com;

  location / {
    proxy_pass http://127.0.0.1:9100;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    client_max_body_size 0;
  }
}
```

后端 `config.yaml` 中设置 `xheaders: true`：

```yaml
xheaders: true
upload: true
delete: true
```

## 调试与排障

### 启用调试日志

编辑 `config.yaml`：

```yaml
debug: true
log-file: "server.log"
```

调试模式会输出：
- 请求的方法、路径、Content-Length
- 上传前/后的内存快照（HeapAlloc、GC 次数）
- Multipart 解析过程
- 文件写入进度（每 5MB 或 5 秒输出一次）
- 写入速度和耗时

### 常见问题

**Q: 上传大文件失败（如 40MB 文件在 128MB 设备上）？**

A: 启用 `debug: true` 查看日志，关注以下几点：
1. `Content-Length` 是否正确接收
2. `HeapAlloc` 是否接近设备内存上限
3. 上传进度日志在哪一步停止（网络超时 / IO 错误 / 内存不足）
4. GC 次数是否异常增多

**Q: `syntax error: unexpected redirection`？**

A: 编译目标平台与运行平台不匹配。Windows 编译的 `.exe` 无法在 Linux 上运行。请使用交叉编译：
```bash
GOOS=linux GOARCH=arm64 go build -o gohttpserver .
```

**Q: 中文文件名乱码？**

A: 确保请求使用 `UTF-8` 编码，前端页面已设置 `<meta charset="utf-8">`。

**Q: 上传超时？**

A: 如果使用 Nginx 反向代理，需在 Nginx 配置中增大 `client_max_body_size` 和 `proxy_read_timeout`。

## 项目结构

```
gohttpserver/
├── main.go              # 入口，配置解析，服务器启动
├── httpstaticserver.go  # 核心：HTTP 处理逻辑（上传、下载、目录列表等）
├── assets.go            # 静态资源嵌入
├── assets/              # 前端资源（HTML、CSS、JS、图标）
├── utils.go             # 工具函数
├── zip.go               # ZIP 压缩/解压
├── ipa.go               # IPA 安装包处理
├── oauth2-proxy.go      # OAuth2 代理认证
├── openid-login.go      # OpenID 认证
├── res.go               # 资源定义
└── config.yaml          # 配置文件（首次运行自动生成）
```

## 技术栈

- 语言：Go
- 路由：[gorilla/mux](https://github.com/gorilla/mux)
- 前端：Vue.js + Bootstrap + jQuery
- 文件图标：EasyIcon
- Markdown 渲染：Showdown

## 开源协议

MIT License