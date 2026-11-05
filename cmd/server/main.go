// weread-server：本地 Web 书单浏览器（兼作桌面程序入口）
// 启动后自动以 Edge 应用模式打开独立窗口（找不到 Edge 时回退 Chrome / 提示手动访问）。
// API：
//
//	GET  /api/booklists                     书单列表
//	GET  /api/books?list=书单名             书籍列表
//	GET  /api/content?list=书单名&title=书名 笔记内容
//	POST /api/save   {list,title,content}   保存笔记修改
//	POST /api/add    {list,title,author,intro} 新增书籍笔记
//	POST /api/delete {list,title}           删除书籍笔记
//	POST /api/rename {list,title,newTitle}  重命名笔记文件
//	POST /api/import multipart{list,file}   导入外部文件（md/txt/html/docx/epub/pdf/json/csv）
//	POST /api/update                      启动数据更新（后台爬取 + 自动重放 supplement 补全）
//	GET  /api/update/status               数据更新进度（前端轮询）
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qianyuniao/Weread-Hot-Booklists/bookapply"
	"github.com/qianyuniao/Weread-Hot-Booklists/scraper"
)

//go:embed index.html
var indexFS embed.FS

//go:embed pic/logo.png
var logoPNG []byte

var (
	addr      = flag.String("addr", ":8327", "监听地址")
	booksDir  = flag.String("dir", "books", "书籍笔记目录（相对于运行目录）")
	noBrowser = flag.Bool("no-browser", false, "不自动打开应用窗口（仅启动服务）")
)

type BooklistItem struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type BookItem struct {
	Title string `json:"title"`
}

// editReq 是写操作（保存/添加/删除/重命名）的通用请求体
type editReq struct {
	List     string `json:"list"`
	Title    string `json:"title"`
	Content  string `json:"content"`
	Author   string `json:"author"`
	Intro    string `json:"intro"`
	NewTitle string `json:"newTitle"`
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// safeJoin 防止路径穿越，确保结果仍在 booksDir 内
func safeJoin(elems ...string) (string, bool) {
	p := filepath.Clean(filepath.Join(append([]string{*booksDir}, elems...)...))
	absBase, _ := filepath.Abs(*booksDir)
	absP, _ := filepath.Abs(p)
	if !strings.HasPrefix(absP, filepath.Clean(absBase)+string(os.PathSeparator)) && absP != filepath.Clean(absBase) {
		return "", false
	}
	return p, true
}

// ---------- 只读 API ----------

func handleBooklists(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(*booksDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "无法读取 books 目录: "+err.Error())
		return
	}
	var lists []BooklistItem
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(*booksDir, e.Name()))
		if err != nil {
			continue
		}
		count := 0
		for _, f := range files {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".md") {
				count++
			}
		}
		lists = append(lists, BooklistItem{Name: e.Name(), Count: count})
	}
	sort.Slice(lists, func(i, j int) bool { return lists[i].Count > lists[j].Count })
	writeJSON(w, lists)
}

func handleBooks(w http.ResponseWriter, r *http.Request) {
	list := r.URL.Query().Get("list")
	if list == "" {
		writeErr(w, http.StatusBadRequest, "缺少 list 参数")
		return
	}
	dir, ok := safeJoin(list)
	if !ok {
		writeErr(w, http.StatusBadRequest, "非法路径")
		return
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		writeErr(w, http.StatusNotFound, "无法读取书单目录")
		return
	}
	var books []BookItem
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
			continue
		}
		books = append(books, BookItem{Title: strings.TrimSuffix(f.Name(), ".md")})
	}
	sort.Slice(books, func(i, j int) bool { return books[i].Title < books[j].Title })
	writeJSON(w, books)
}

func handleContent(w http.ResponseWriter, r *http.Request) {
	list := r.URL.Query().Get("list")
	title := r.URL.Query().Get("title")
	if list == "" || title == "" {
		writeErr(w, http.StatusBadRequest, "缺少参数")
		return
	}
	p, ok := safeJoin(list, title+".md")
	if !ok {
		writeErr(w, http.StatusBadRequest, "非法路径")
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		writeErr(w, http.StatusNotFound, "文件不存在")
		return
	}
	writeJSON(w, map[string]string{"markdown": string(data)})
}

// handleWindow 供菜单栏 — ▢ ✕ 控制应用窗口（winOp 直接调用 user32.dll，无子进程）
func handleWindow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req struct {
		Op string `json:"op"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	switch req.Op {
	case "min", "max", "restore", "close":
	default:
		writeErr(w, http.StatusBadRequest, "未知操作")
		return
	}
	if err := winOp(req.Op); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------- 写操作 API ----------

// decodeEditReq 解析 POST JSON 请求体（限制 8MB）
func decodeEditReq(w http.ResponseWriter, r *http.Request) (*editReq, bool) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	var req editReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return nil, false
	}
	if req.List == "" || req.Title == "" {
		writeErr(w, http.StatusBadRequest, "list / title 不能为空")
		return nil, false
	}
	return &req, true
}

// handleSave 覆盖保存已有笔记文件
func handleSave(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeEditReq(w, r)
	if !ok {
		return
	}
	p, ok := safeJoin(req.List, req.Title+".md")
	if !ok {
		writeErr(w, http.StatusBadRequest, "非法路径")
		return
	}
	if _, err := os.Stat(p); err != nil {
		writeErr(w, http.StatusNotFound, "笔记文件不存在")
		return
	}
	if err := os.WriteFile(p, []byte(req.Content), 0644); err != nil {
		writeErr(w, http.StatusInternalServerError, "写入失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

var illegalChars = regexp.MustCompile(`[\\/:*?"<>|]`)

// handleAdd 在指定书单下新建笔记文件（文件名过滤 Windows 非法字符，同名拒绝）
func handleAdd(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeEditReq(w, r)
	if !ok {
		return
	}
	title := strings.TrimSpace(illegalChars.ReplaceAllString(req.Title, "-"))
	if title == "" {
		writeErr(w, http.StatusBadRequest, "书名不能为空")
		return
	}
	dir, ok := safeJoin(req.List)
	if !ok {
		writeErr(w, http.StatusBadRequest, "非法路径")
		return
	}
	if _, err := os.Stat(dir); err != nil {
		writeErr(w, http.StatusNotFound, "书单不存在")
		return
	}
	p, _ := safeJoin(req.List, title+".md")
	if _, err := os.Stat(p); err == nil {
		writeErr(w, http.StatusConflict, "书单中已存在同名笔记")
		return
	}
	author := strings.TrimSpace(req.Author)
	if author == "" {
		author = "未知"
	}
	intro := strings.TrimSpace(req.Intro)
	if intro == "" {
		intro = "（待填写简介）"
	}
	md := fmt.Sprintf("## %s\n\n\n%s - 手动添加\n\n\n> %s\n\n\n### 划线\n\n（在这里记录你的划线与想法）\n", title, author, intro)
	if err := os.WriteFile(p, []byte(md), 0644); err != nil {
		writeErr(w, http.StatusInternalServerError, "创建失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]string{"ok": "true", "title": title})
}

// handleDelete 删除笔记文件（不可恢复，前端已有二次确认）
func handleDelete(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeEditReq(w, r)
	if !ok {
		return
	}
	p, ok := safeJoin(req.List, req.Title+".md")
	if !ok {
		writeErr(w, http.StatusBadRequest, "非法路径")
		return
	}
	if _, err := os.Stat(p); err != nil {
		writeErr(w, http.StatusNotFound, "笔记文件不存在")
		return
	}
	if err := os.Remove(p); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleRename 重命名笔记文件：过滤非法字符、拒绝同名，并同步更新正文的 ## 标题行
func handleRename(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeEditReq(w, r)
	if !ok {
		return
	}
	newTitle := strings.TrimSpace(illegalChars.ReplaceAllString(req.NewTitle, "-"))
	if newTitle == "" {
		writeErr(w, http.StatusBadRequest, "新名称不能为空")
		return
	}
	oldP, ok := safeJoin(req.List, req.Title+".md")
	if !ok {
		writeErr(w, http.StatusBadRequest, "非法路径")
		return
	}
	newP, ok := safeJoin(req.List, newTitle+".md")
	if !ok {
		writeErr(w, http.StatusBadRequest, "非法路径")
		return
	}
	if oldP == newP {
		writeJSON(w, map[string]string{"ok": "true", "title": newTitle})
		return
	}
	data, err := os.ReadFile(oldP)
	if err != nil {
		writeErr(w, http.StatusNotFound, "笔记文件不存在")
		return
	}
	if _, err := os.Stat(newP); err == nil {
		writeErr(w, http.StatusConflict, "书单中已存在同名笔记")
		return
	}
	content := strings.Replace(string(data), "## "+req.Title+"\n", "## "+newTitle+"\n", 1)
	if err := os.Rename(oldP, newP); err != nil {
		writeErr(w, http.StatusInternalServerError, "重命名失败: "+err.Error())
		return
	}
	if err := os.WriteFile(newP, []byte(content), 0644); err != nil {
		writeErr(w, http.StatusInternalServerError, "写入失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]string{"ok": "true", "title": newTitle})
}

// ---------- 数据更新任务（爬取 + 重放补全） ----------

type updateJob struct {
	Active  bool   `json:"active"`
	Phase   string `json:"phase"` // scrape / apply / done / error
	Error   string `json:"error,omitempty"`
	Applied int    `json:"applied"`
	Batches int    `json:"batches"`
}

var (
	updateMu  sync.Mutex
	updateCur updateJob
)

// handleUpdate 启动后台数据更新：先跑爬虫（temp 缓存命中则离线完成），完成后自动重放补全批次
func handleUpdate(w http.ResponseWriter, r *http.Request) {
	updateMu.Lock()
	if updateCur.Active {
		updateMu.Unlock()
		writeErr(w, http.StatusConflict, "数据更新已在进行中")
		return
	}
	updateCur = updateJob{Active: true, Phase: "scrape"}
	updateMu.Unlock()
	go runUpdate()
	writeJSON(w, map[string]string{"ok": "true"})
}

func runUpdate() {
	fail := func(msg string) {
		updateMu.Lock()
		updateCur.Active = false
		updateCur.Phase = "error"
		updateCur.Error = msg
		updateMu.Unlock()
	}
	defer func() {
		if rec := recover(); rec != nil { // 双保险：防意外 panic 击穿服务进程
			fail(fmt.Sprintf("%v", rec))
		}
	}()
	if err := scraper.Run("."); err != nil {
		fail(err.Error())
		return
	}
	updateMu.Lock()
	updateCur.Phase = "apply"
	updateMu.Unlock()
	files, _ := filepath.Glob("supplement/batch*.md")
	sort.Strings(files)
	total := 0
	for _, f := range files {
		a, _, e := bookapply.ApplyBatch(".", f)
		if e == nil {
			total += a
		}
	}
	updateMu.Lock()
	updateCur.Active = false
	updateCur.Phase = "done"
	updateCur.Applied = total
	updateCur.Batches = len(files)
	updateMu.Unlock()
}

// handleUpdateStatus 返回任务总状态 + 爬虫细粒度进度
func handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	updateMu.Lock()
	cur := updateCur
	updateMu.Unlock()
	writeJSON(w, map[string]interface{}{
		"active":   cur.Active,
		"phase":    cur.Phase,
		"error":    cur.Error,
		"applied":  cur.Applied,
		"batches":  cur.Batches,
		"progress": scraper.Snapshot(),
	})
}

// ---------- 应用窗口 ----------

// openAppWindow 以 Edge（回退 Chrome）应用模式打开独立窗口，
// 使用独立 user-data-dir 保证不与日常浏览器共享进程。
func openAppWindow(u string) {
	home, _ := os.UserHomeDir()
	profile := filepath.Join(home, ".weread-booklists-app")
	var exes []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
		if base := os.Getenv(env); base != "" {
			exes = append(exes, filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`))
		}
	}
	exes = append(exes, "msedge", "chrome")
	for _, e := range exes {
		cmd := exec.Command(e, "--app="+u, "--window-size=1320,860", "--user-data-dir="+profile)
		if err := cmd.Start(); err == nil {
			log.Println("已打开应用窗口:", e)
			return
		}
	}
	log.Println("未找到 Edge/Chrome，请手动用浏览器访问", u)
}

// waitReady 轮询监听地址，确认服务就绪后再开窗口
func waitReady(addrPort string) {
	for i := 0; i < 50; i++ {
		conn, err := net.Dial("tcp", addrPort)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func main() {
	flag.Parse()
	if _, err := os.Stat(*booksDir); err != nil {
		log.Fatalf("找不到 %s 目录，请在项目根目录下运行本程序", *booksDir)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		page, err := indexFS.ReadFile("index.html")
		if err != nil {
			http.Error(w, "页面缺失", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(page)
	})
	mux.HandleFunc("/api/booklists", handleBooklists)
	mux.HandleFunc("/api/books", handleBooks)
	mux.HandleFunc("/api/content", handleContent)
	mux.HandleFunc("/api/save", handleSave)
	mux.HandleFunc("/api/add", handleAdd)
	mux.HandleFunc("/api/delete", handleDelete)
	mux.HandleFunc("/api/rename", handleRename)
	mux.HandleFunc("/api/import", handleImport)
	mux.HandleFunc("/api/update", handleUpdate)
	mux.HandleFunc("/api/update/status", handleUpdateStatus)
	mux.HandleFunc("/api/window", handleWindow)
	mux.HandleFunc("/logo.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(logoPNG)
	})

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("监听 %s 失败（端口被占用？）: %v", *addr, err)
	}
	url := fmt.Sprintf("http://localhost:%d", ln.Addr().(*net.TCPAddr).Port)
	log.Println("Weread Hot Booklists 已启动:", url)

	go func() { log.Fatal(http.Serve(ln, mux)) }()
	if !*noBrowser {
		waitReady(ln.Addr().String())
		openAppWindow(url)
	}
	select {}
}
