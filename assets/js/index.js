jQuery('#qrcodeCanvas').qrcode({
  text: "http://jetienne.com/"
});

Dropzone.autoDiscover = false;

function getExtention(fname) {
  return fname.slice((fname.lastIndexOf(".") - 1 >>> 0) + 2);
}

function pathJoin(parts, sep) {
  var separator = sep || '/';
  var replace = new RegExp(separator + '{1,}', 'g');
  return parts.join(separator).replace(replace, separator);
}

function getQueryString(name) {
  var reg = new RegExp("(^|&)" + name + "=([^&]*)(&|$)");
  var r = decodeURI(window.location.search).substr(1).match(reg);
  if (r != null) return r[2].replace(/\+/g, ' ');
  return null;
}

function checkPathNameLegal(name) {
  var reg = new RegExp("[\\/:*<>|]");
  var r = name.match(reg)
  return r == null;
}

function showErrorMessage(jqXHR) {
  let errMsg = jqXHR.getResponseHeader("x-auth-authentication-message")
  if (errMsg == null) {
    errMsg = jqXHR.responseText
  }
  alert(String(jqXHR.status).concat(":", errMsg));
  console.error(errMsg)
}

var vm = new Vue({
  el: "#app",
  data: {
    user: {
      email: "",
      name: "",
    },
    location: window.location,
    breadcrumb: [],
    showHidden: false,
    loading: true,
    previewMode: false,
    preview: {
      filename: '',
      filetype: '',
      filesize: 0,
      contentHTML: '',
    },
    version: "loading",
    mtimeTypeFromNow: false,
    auth: {},
    search: getQueryString("search"),
    files: [],
    myDropzone: null,
  },
  computed: {
    computedFiles: function () {
      var that = this;
      var files = this.files.filter(function (f) {
        if (!that.showHidden && f.name.slice(0, 1) === '.') {
          return false;
        }
        return true;
      });
      return files;
    },
  },
  watch: {
    files: function (newFiles) {
      console.log('[DEBUG] 文件列表更新: 数量=' + newFiles.length);
      this.preview.filename = '';
      for (var i = 0; i < newFiles.length; i++) {
        if (newFiles[i].name === 'README.md') {
          this.preview.filename = newFiles[i].name;
          this.fetchReadme(newFiles[i].name);
          break;
        }
      }
    },
  },
  created: function () {
    console.log('[DEBUG] Vue实例创建完成, 开始加载用户信息');
    $.ajax({
      url: "/-/user",
      method: "get",
      dataType: "json",
      success: function (ret) {
        console.log('[DEBUG] 用户信息返回:', ret);
        if (ret) {
          this.user.email = ret.email;
          this.user.name = ret.name;
        }
      }.bind(this),
      error: function (err) {
        console.log('[WARN] 用户信息获取失败:', err.status, err.responseText);
      }
    });
    this.myDropzone = new Dropzone("#upload-form", {
      paramName: "file",
      maxFilesize: 10240,
      addRemoveLinks: true,
      init: function () {
        this.on("uploadprogress", function (file, progress) {
          console.log('[DEBUG] 上传进度:', file.name, Math.round(progress) + '%');
        });
        this.on("complete", function (file) {
          console.log('[DEBUG] 上传完成:', file.name);
          loadFileList()
        });
      }
    });
  },
  methods: {
    fetchReadme: function (filename) {
      console.log('[DEBUG] 获取README文件内容:', filename);
      var that = this;
      $.ajax({
        url: pathJoin([location.pathname, filename]),
        method: 'GET',
        success: function (res) {
          var converter = new showdown.Converter({
            noHTML: true,
            tables: true,
            omitExtraWLInCodeBlocks: true,
            parseImgDimensions: true,
            simplifiedAutoLink: true,
            literalMidWordUnderscores: true,
            tasklists: true,
            ghCodeBlocks: true,
            smoothLivePreview: true,
            simplifiedAutoLink: true,
            strikethrough: true,
          });
          var html = converter.makeHtml(res);
          that.preview.contentHTML = html;
          console.log('[DEBUG] README渲染完成, 内容长度=' + html.length);
        },
        error: function (err) {
          console.log('[DEBUG] README获取失败(可能不存在):', err.status);
        }
      });
    },
    getEncodePath: function (filepath) {
      return pathJoin([location.pathname].concat(filepath.split("/").map(v => encodeURIComponent(v))))
    },
    formatTime: function (timestamp) {
      var m = moment.utc(timestamp).utcOffset(8 * 60);
      if (this.mtimeTypeFromNow) {
        return m.fromNow();
      }
      return m.format('YYYY-MM-DD HH:mm:ss');
    },
    toggleHidden: function () {
      this.showHidden = !this.showHidden;
      console.log('[DEBUG] 切换显示隐藏文件:', this.showHidden);
    },
    removeAllUploads: function () {
      this.myDropzone.removeAllFiles();
    },
    parentDirectory: function (path) {
      return path.replace('\\', '/').split('/').slice(0, -1).join('/')
    },
    changeParentDirectory: function (path) {
      var parentDir = this.parentDirectory(path);
      loadFileOrDir(parentDir);
    },
    genInstallURL: function (name, noEncode) {
      var parts = [location.host];
      var pathname = decodeURI(location.pathname);
      if (!name) {
        parts.push(pathname);
      } else if (getExtention(name) == "ipa") {
        parts.push("/-/ipa/link", pathname, encodeURIComponent(name));
      } else {
        parts.push(pathname, name);
      }
      var urlPath = location.protocol + "//" + pathJoin(parts);
      return noEncode ? urlPath : encodeURI(urlPath);
    },
    genQrcode: function (name, title) {
      var urlPath = this.genInstallURL(name, true);
      $("#qrcode-title").html(title || name || location.pathname);
      $("#qrcode-link").attr("href", urlPath);
      $('#qrcodeCanvas').empty().qrcode({
        text: encodeURI(urlPath),
      });

      $("#qrcodeRight a").attr("href", urlPath);
      $("#qrcode-modal").modal("show");
    },
    genDownloadURL: function (f) {
      var search = location.search;
      var sep = search == "" ? "?" : "&"
      return location.origin + this.getEncodePath(f.name) + location.search + sep + "download=true";
    },
    shouldHaveQrcode: function (name) {
      return ['apk', 'ipa'].indexOf(getExtention(name)) !== -1;
    },
    genFileClass: function (f) {
      if (f.type == "dir") {
        if (f.name == '.git') {
          return 'fa-git-square';
        }
        return "fa-folder-open";
      }
      var ext = getExtention(f.name);
      switch (ext) {
        case "go":
        case "py":
        case "js":
        case "java":
        case "c":
        case "cpp":
        case "h":
          return "fa-file-code-o";
        case "pdf":
          return "fa-file-pdf-o";
        case "zip":
          return "fa-file-zip-o";
        case "mp3":
        case "wav":
          return "fa-file-audio-o";
        case "jpg":
        case "png":
        case "gif":
        case "jpeg":
        case "tiff":
          return "fa-file-picture-o";
        case "ipa":
        case "dmg":
          return "fa-apple";
        case "apk":
          return "fa-android";
        case "exe":
          return "fa-windows";
      }
      return "fa-file-text-o"
    },
    clickFileOrDir: function (f, e) {
      var reqPath = this.getEncodePath(f.name)
      if (f.type == "file") {
        var videoExtensions = ['mp4', 'webm', 'ogg', 'mov', 'avi', 'mkv'];
        var fileExtension = getExtention(f.name).toLowerCase();
        if (videoExtensions.includes(fileExtension)) {
          window.location.href = '/-/video-player' + reqPath;
        } else {
          window.location.href = reqPath;
        }
        e.preventDefault()
        return;
      }
      loadFileOrDir(reqPath);
      e.preventDefault()
    },
    changePath: function (reqPath, e) {
      loadFileOrDir(reqPath);
      e.preventDefault()
    },
    showInfo: function (f) {
      console.log('[DEBUG] 请求文件信息:', f.name);
      $.ajax({
        url: this.getEncodePath(f.name),
        data: {
          op: "info",
        },
        method: "GET",
        success: function (res) {
          console.log('[DEBUG] 文件信息返回:', res);
          $("#file-info-title").text(f.name);
          $("#file-info-content").text(JSON.stringify(res, null, 4));
          $("#file-info-modal").modal("show");
        },
        error: function (jqXHR, textStatus, errorThrown) {
          console.log('[ERROR] 文件信息获取失败:', jqXHR.status, jqXHR.responseText);
          showErrorMessage(jqXHR)
        }
      })
    },
    makeDirectory: function () {
      var name = window.prompt("当前路径: " + location.pathname + "\n请输入新目录名称", "")
      if (!name) {
        return
      }
      if(!checkPathNameLegal(name)) {
        alert("名称不能包含 \\/:*<>| 等特殊字符")
        return
      }
      console.log('[DEBUG] 创建目录:', name, '当前路径:', location.pathname);
      $.ajax({
        url: this.getEncodePath(name),
        method: "POST",
        success: function (res) {
          console.log('[DEBUG] 目录创建结果:', res);
          loadFileList()
        },
        error: function (jqXHR, textStatus, errorThrown) {
          console.log('[ERROR] 目录创建失败:', jqXHR.status, jqXHR.responseText);
          showErrorMessage(jqXHR)
        }
      })
    },
    deletePathConfirm: function (f, e) {
      e.preventDefault();
      if (!e.altKey) {
        if (!window.confirm("确定删除 " + f.name + " ？")) {
          return;
        }
      }
      console.log('[DEBUG] 删除文件/目录:', f.name, '路径:', f.path);
      $.ajax({
        url: this.getEncodePath(f.name),
        method: 'DELETE',
        success: function (res) {
          console.log('[DEBUG] 删除结果:', res);
          loadFileList()
        },
        error: function (jqXHR, textStatus, errorThrown) {
          console.log('[ERROR] 删除失败:', jqXHR.status, jqXHR.responseText);
          showErrorMessage(jqXHR)
        }
      });
    },
    updateBreadcrumb: function (pathname) {
      var pathname = decodeURI(pathname || location.pathname || "/");
      pathname = pathname.split('?')[0]
      var parts = pathname.split('/');
      this.breadcrumb = [];
      if (pathname == "/") {
        console.log('[DEBUG] 根路径, 面包屑为空');
        return this.breadcrumb;
      }
      var i = 2;
      for (; i <= parts.length; i += 1) {
        var name = parts[i - 1];
        if (!name) {
          continue;
        }
        var path = parts.slice(0, i).join('/');
        this.breadcrumb.push({
          name: name + (i == parts.length ? ' /' : ''),
          path: path
        })
      }
      console.log('[DEBUG] 面包屑更新:', this.breadcrumb.map(function(b){return b.name}).join(' > '));
      return this.breadcrumb;
    },
    loadPreviewFile: function (filepath, e) {
      if (e) {
        e.preventDefault()
      }
      var that = this;
      $.getJSON(pathJoin(['/-/info', location.pathname]))
          .then(function (res) {
            console.log('[DEBUG] 预览文件信息:', res);
            that.preview.filename = res.name;
            that.preview.filesize = res.size;
            return $.ajax({
              url: '/' + res.path,
              dataType: 'text',
            });
          })
          .then(function (res) {
            console.log('[DEBUG] 预览文件内容加载完成, 长度=' + res.length);
            that.preview.contentHTML = '<pre>' + res + '</pre>';
          })
          .done(function (res) {
            console.log('[DEBUG] 文件预览加载完成');
          });
    },
    loadAll: function () {
    },
  }
})

window.onpopstate = function (event) {
  console.log('[DEBUG] 浏览器历史导航触发, 搜索参数:', getQueryString("search"));
  if (location.search.match(/\?search=/)) {
    location.reload();
    return;
  }
  loadFileList()
}

function loadFileOrDir(reqPath) {
  console.log('[DEBUG] 导航到:', reqPath);
  let requestUri = reqPath + location.search
  var retObj = loadFileList(requestUri)
  if (retObj !== null) {
    retObj.done(function () {
      window.history.pushState({}, "", requestUri);
    });
  }
}

function loadFileList(pathname) {
  var pathname = pathname || location.pathname + location.search;
  console.log('[DEBUG] loadFileList:', pathname);
  var retObj = null
  if (getQueryString("raw") !== "false") {
    vm.loading = true;
    var sep = pathname.indexOf("?") === -1 ? "?" : "&"
    retObj = $.ajax({
      url: pathname + sep + "json=true",
      dataType: "json",
      cache: false,
      success: function (res) {
        console.log('[DEBUG] 文件列表返回: 路径=' + pathname + ' 文件数=' + res.files.length + ' 权限=', res.auth);
        res.files = _.sortBy(res.files, function (f) {
          var weight = f.type == 'dir' ? 1000 : 1;
          return -weight * f.mtime;
        })
        vm.files = res.files;
        vm.auth = res.auth;
        vm.updateBreadcrumb(pathname);
        vm.loading = false;
      },
      error: function (jqXHR, textStatus, errorThrown) {
        console.log('[ERROR] 文件列表加载失败:', jqXHR.status, jqXHR.responseText);
        vm.loading = false;
        showErrorMessage(jqXHR)
      },
    });
  }

  vm.previewMode = getQueryString("raw") == "false";
  if (vm.previewMode) {
    vm.loadPreviewFile();
  }
  return retObj
}

Vue.filter('fromNow', function (value) {
  return moment.utc(value).utcOffset(8 * 60).fromNow();
})

Vue.filter('formatBytes', function (value) {
  var bytes = parseFloat(value);
  if (bytes < 0) return "-";
  else if (bytes < 1024) return bytes + " B";
  else if (bytes < 1048576) return (bytes / 1024).toFixed(0) + " KB";
  else if (bytes < 1073741824) return (bytes / 1048576).toFixed(1) + " MB";
  else return (bytes / 1073741824).toFixed(1) + " GB";
})

$(function () {
  $.scrollUp({
    scrollText: '',
  });

  console.log('[DEBUG] 页面初始化, 开始加载文件列表:', location.pathname);
  loadFileList(location.pathname + location.search)

  $.getJSON("/-/sysinfo", function (res) {
    console.log('[DEBUG] 系统信息返回:', res);
    vm.version = res.version;
  })

  var clipboard = new Clipboard('.btn');
  clipboard.on('success', function (e) {
    console.info('Action:', e.action);
    console.info('Text:', e.text);
    console.info('Trigger:', e.trigger);
    $(e.trigger)
        .tooltip('show')
        .mouseleave(function () {
          $(this).tooltip('hide');
        })

    e.clearSelection();
  });
});