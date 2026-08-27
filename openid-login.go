package main

import (
	"encoding/gob"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	openid "github.com/codeskyblue/openid-go"
	"github.com/gorilla/sessions"
)

var (
	nonceStore         = openid.NewSimpleNonceStore()
	discoveryCache     = openid.NewSimpleDiscoveryCache()
	store              = sessions.NewCookieStore([]byte("something-very-secret"))
	defaultSessionName = "ghs-session"
)

type UserInfo struct {
	Id       string `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	NickName string `json:"nickName"`
}

type M map[string]interface{}

func init() {
	gob.Register(&UserInfo{})
	gob.Register(&M{})
}

func handleOpenID(loginUrl string, secure bool) {
	http.HandleFunc("/-/login", func(w http.ResponseWriter, r *http.Request) {
		nextUrl := r.FormValue("next")
		referer := r.Referer()
		if nextUrl == "" && strings.Contains(referer, "://"+r.Host) {
			nextUrl = referer
		}
		scheme := "http"
		if r.URL.Scheme != "" {
			scheme = r.URL.Scheme
		}
	debugLog("OpenID登录请求: 协议=%s 主机=%s 回调地址=%s", scheme, r.Host, nextUrl)
		if url, err := openid.RedirectURL(loginUrl,
			scheme+"://"+r.Host+"/-/openidcallback?next="+nextUrl, ""); err == nil {
			http.Redirect(w, r, url, 303)
		} else {
			errorLog("OpenID重定向失败: %v", err)
		}
	})

	http.HandleFunc("/-/openidcallback", func(w http.ResponseWriter, r *http.Request) {
		id, err := openid.Verify("http://"+r.Host+r.URL.String(), discoveryCache, nonceStore)
		if err != nil {
			errorLog("OpenID验证失败: %v", err)
			io.WriteString(w, "身份验证失败")
			return
		}
		debugLog("OpenID验证成功: 用户ID=%s", id)
		session, err := store.Get(r, defaultSessionName)
		if err != nil {
			errorLog("获取会话失败: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		user := &UserInfo{
			Id:       id,
			Email:    r.FormValue("openid.sreg.email"),
			Name:     r.FormValue("openid.sreg.fullname"),
			NickName: r.FormValue("openid.sreg.nickname"),
		}
		session.Values["user"] = user
		if err := session.Save(r, w); err != nil {
			errorLog("保存会话失败: %v", err)
		}
		infoLog("用户登录成功: 用户ID=%s 昵称=%s", user.Id, user.NickName)

		nextUrl := r.FormValue("next")
		if nextUrl == "" {
			nextUrl = "/"
		}
		http.Redirect(w, r, nextUrl, 302)
	})

	http.HandleFunc("/-/user", func(w http.ResponseWriter, r *http.Request) {
		session, err := store.Get(r, defaultSessionName)
		if err != nil {
			errorLog("获取用户会话失败: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		val := session.Values["user"]
		debugLog("获取用户信息: %v", val)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		data, _ := json.Marshal(val)
		w.Write(data)
	})

	http.HandleFunc("/-/logout", func(w http.ResponseWriter, r *http.Request) {
		session, err := store.Get(r, defaultSessionName)
		if err != nil {
			errorLog("获取登出会话失败: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		delete(session.Values, "user")
		session.Options.MaxAge = -1
		nextUrl := r.FormValue("next")
		_ = session.Save(r, w)
		infoLog("用户登出成功")
		if nextUrl == "" {
			nextUrl = r.Referer()
		}
		http.Redirect(w, r, nextUrl, 302)
	})
}