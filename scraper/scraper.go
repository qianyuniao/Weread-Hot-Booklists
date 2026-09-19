// Package scraper 微信读书书单抓取核心逻辑（从 cmd/scraper 抽取，供 CLI 与 weread-server 复用）。
//
// 与命令行版的关键差异：
//   - 不再使用 log.Fatal/退出进程——致命错误统一 panic，由 Run() recover 成 error 返回，
//     保证在 server 进程内调用失败时只终止本次任务、不影响服务本身；
//   - 所有路径相对 root 参数解析（server 传 "."，即项目根目录）；
//   - 内置任务状态（阶段/书单进度/书籍进度/滚动日志），供 /api/update/status 轮询。
package scraper

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 数据模型（微信读书 API 响应） ----------

type booklistResp struct {
	Booklist struct {
		Booklist struct {
			Name         string   `json:"name"`
			BookIds      []string `json:"bookIds"`
			Description  string   `json:"description"`
			BooklistId   string   `json:"booklistId"`
			TotalCount   int      `json:"totalCount"`
			CollectCount int      `json:"collectCount"`
		} `json:"booklist"`
	} `json:"booklist"`
}

type booklistsResp struct {
	Booklists []struct {
		BooklistId   string `json:"booklistId"`
		CollectCount int    `json:"collectCount"`
		Name         string `json:"name"`
		Description  string `json:"description"`
		TotalCount   int    `json:"totalCount"`
	} `json:"booklists"`
}

// BookInfo 书籍元信息
type BookInfo struct {
	BookId         string `json:"bookId"`
	Title          string `json:"title"`
	Author         string `json:"author"`
	Cover          string `json:"cover"`
	Category       string `json:"category"`
	Intro          string `json:"intro"`
	Publisher      string `json:"publisher"`
	PublishTime    string `json:"publishTime"`
	BookSize       int    `json:"bookSize"`
	Star           int    `json:"star"`
	NewRatingCount int    `json:"newRatingCount"`
}

type bestBookMarkItem struct {
	Bookid     string `json:"bookId"`
	Uservid    string `json:"userVid"`
	Bookmarkid string `json:"bookmarkId"`
	Chapteruid int    `json:"chapterUid"`
	Range      string `json:"range"`
	Marktext   string `json:"markText"`
	Totalcount int    `json:"totalCount"`
}

type bestBookMarks struct {
	Totalcount string             `json:"totalCount"`
	Items      []bestBookMarkItem `json:"items"`
	Chapters   []struct {
		Bookid     string `json:"bookId"`
		Chapteruid int    `json:"chapterUid"`
		Chapteridx int    `json:"chapterIdx"`
		Title      string `json:"title"`
	} `json:"chapters"`
}

// ---------- 任务状态 ----------

// Status 抓取任务进度（JSON 供前端轮询）
type Status struct {
	Running    bool     `json:"running"`
	Phase      string   `json:"phase"`
	ListsDone  int      `json:"listsDone"`
	ListsTotal int      `json:"listsTotal"`
	BooksDone  int      `json:"booksDone"`
	Error      string   `json:"error,omitempty"`
	Logs       []string `json:"logs,omitempty"`
}

var (
	mu sync.Mutex
	st Status
)

// Running 是否有抓取任务进行中
func Running() bool {
	mu.Lock()
	defer mu.Unlock()
	return st.Running
}

// Snapshot 返回当前状态副本
func Snapshot() Status {
	mu.Lock()
	defer mu.Unlock()
	cp := st
	cp.Logs = append([]string(nil), st.Logs...)
	return cp
}

func start() bool {
	mu.Lock()
	defer mu.Unlock()
	if st.Running {
		return false
	}
	st = Status{Running: true, Phase: "准备中"}
	return true
}

func finish(err error) {
	mu.Lock()
	defer mu.Unlock()
	st.Running = false
	if err != nil {
		st.Phase = "失败"
		st.Error = err.Error()
	} else {
		st.Phase = "完成"
	}
}

func setPhase(p string) {
	mu.Lock()
	defer mu.Unlock()
	st.Phase = p
}

func logf(format string, a ...interface{}) {
	line := fmt.Sprintf(format, a...)
	mu.Lock()
	defer mu.Unlock()
	st.Logs = append(st.Logs, time.Now().Format("15:04:05")+" "+line)
	if len(st.Logs) > 200 {
		st.Logs = st.Logs[len(st.Logs)-200:]
	}
}

// ---------- 入口 ----------

// Run 执行一次完整抓取（生成 INDEX.md 与 books/ 笔记），root 为项目根目录（含 temp/、cookie.txt）。
// 同一时刻只允许一个任务；致命错误以 error 返回，不会终止宿主进程。
func Run(root string) (err error) {
	if !start() {
		return fmt.Errorf("抓取任务已在运行中")
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
		finish(err)
	}()
	check(os.MkdirAll(filepath.Join(root, "temp"), 0755))
	check(os.MkdirAll(filepath.Join(root, "books"), 0755))
	getBookLists(root)
	return nil
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func getBookLists(root string) {
	result, err := getAndSaveResponse(root, "booklists", "https://i.weread.qq.com/market/booklists?count=200&type=0")
	check(err)
	var booklists booklistsResp
	json.Unmarshal([]byte(result), &booklists)

	mu.Lock()
	st.ListsTotal = len(booklists.Booklists)
	mu.Unlock()

	f, err := os.Create(filepath.Join(root, "INDEX.md"))
	check(err)
	defer f.Close()
	fmt.Fprintln(f, "# 微信读书热门收藏书单")

	for index, booklist := range booklists.Booklists {
		logf("%d %s 收藏%d 共%d本", index, booklist.Name, booklist.CollectCount, booklist.TotalCount)
		mu.Lock()
		st.Phase = "抓取书单：" + booklist.Name
		st.ListsDone = index
		mu.Unlock()
		fmt.Fprintln(f, "\n###", strconv.Itoa(index+1), booklist.Name)
		fmt.Fprintln(f, "\n"+booklist.Description)

		bookIds, err := getBookList(root, booklist.BooklistId)
		check(err)
		dirName := safeFileName(booklist.Name)
		check(os.MkdirAll(filepath.Join(root, "books", dirName), 0755))
		fmt.Fprintln(f, "\n<details>\n<summary>"+strconv.Itoa(booklist.TotalCount)+" books, "+strconv.Itoa(booklist.CollectCount)+" likes"+"</summary>")
		for idx, bookId := range bookIds {
			if idx > 30 { // 每个书单只抓前 31 本（与原 CLI 行为一致）
				break
			}
			bookInfo := getBookInfo(root, bookId)
			fileName := safeFileName(bookInfo.Title)
			fmt.Fprintf(f, "\n1. [%s](books/%s/%s.md)", bookInfo.Title, dirName, strings.ReplaceAll(fileName, " ", "%20"))

			bestMark := getBestBookMarks(root, bookInfo, dirName)
			if bestMark.Totalcount > 300 {
				fmt.Fprintln(f, "\n\t> "+strconv.Itoa(bestMark.Totalcount)+" "+bestMark.Marktext)
			}
			mu.Lock()
			st.BooksDone++
			mu.Unlock()
		}
		fmt.Fprintln(f, "\n</details>")
	}
}

func getBookInfo(root, bookId string) BookInfo {
	result, err := getAndSaveResponse(root, "bookinfo/"+bookId, "https://i.weread.qq.com/book/info?bookId="+bookId)
	check(err)
	var bookInfo BookInfo
	json.Unmarshal([]byte(result), &bookInfo)
	bookInfo.Title = sanitizeText(bookInfo.Title)
	bookInfo.Author = sanitizeText(bookInfo.Author)
	bookInfo.Intro = sanitizeText(bookInfo.Intro)
	return bookInfo
}

// missingVerbRe 匹配上游脏数据里的 Go 格式化残留：%!X(MISSING)，它原本是“%20X”即“ 空格+X”
var missingVerbRe = regexp.MustCompile(`%!(.)\(MISSING\)`)

// sanitizeText 清洗微信读书源数据中的标题乱码：
// 1. %!X(MISSING) → " X"（历史版本把书名当 fmt 格式串导致的损坏）
// 2. %20 → 空格（URL 编码残留）
func sanitizeText(s string) string {
	s = missingVerbRe.ReplaceAllString(s, " $1")
	s = strings.ReplaceAll(s, "%20", " ")
	s = strings.ReplaceAll(s, "  ", " ")
	return strings.TrimSpace(s)
}

// fileIllegalRe 是 Windows 下除 "/" 外的文件名非法字符（"/" 单独映射为 "-"）
var fileIllegalRe = regexp.MustCompile(`[\\:*?"<>|]`)

// multiSpaceRe 折叠文件名中的连续空格/制表符
var multiSpaceRe = regexp.MustCompile(`[ \t]+`)

// safeFileName 把书名/书单名转成 Windows 合法文件名，规则与上游既有数据对齐：
//   - "/" → "-"，: \ * ? " < > | 等非法字符 → "_"（如「太阳照常升起:The Sun...」→「太阳照常升起_The Sun...」）
//   - 连续空格折叠为一个（书单名里常见「a/三空格b」写法）
//   - 去除首尾空白与结尾圆点
//
// 若不清洗，Windows 会把 ":" 解释为 NTFS 交替数据流分隔符，静默产出 0 字节垃圾文件；
// 空格不折叠则会像「经济-金融类   书单」那样生成重复目录。
func safeFileName(s string) string {
	s = sanitizeText(s)
	s = strings.ReplaceAll(s, "/", "-")
	s = fileIllegalRe.ReplaceAllString(s, "_")
	s = multiSpaceRe.ReplaceAllString(s, " ")
	return strings.Trim(s, " .")
}

func getBookList(root, booklistId string) ([]string, error) {
	result, err := getAndSaveResponse(root, "booklist/"+booklistId, "https://i.weread.qq.com/booklist/single?booklistId="+booklistId)
	check(err)
	var booklist booklistResp
	json.Unmarshal([]byte(result), &booklist)

	bl := booklist.Booklist.Booklist
	return bl.BookIds, nil
}

// getAndSaveResponse 读写 temp/<类别>/<id>.json 缓存；缓存缺失时凭 cookie.txt 实时抓取
func getAndSaveResponse(root, id, URL string) ([]byte, error) {
	fileName := filepath.Join(root, "temp", filepath.FromSlash(id)+".json")
	if _, err := os.Stat(fileName); os.IsNotExist(err) {
		check(os.MkdirAll(filepath.Dir(fileName), 0755))
		rawCookies, err := os.ReadFile(filepath.Join(root, "cookie.txt"))
		if err != nil {
			panic(fmt.Errorf("缓存缺失且无 cookie.txt，无法实时抓取（%s）：请补齐 temp/ 缓存或配置登录凭证", id))
		}

		rawRequest := fmt.Sprintf("GET / HTTP/1.0\r\n%s\r\n\r\n", rawCookies)
		req, _ := http.ReadRequest(bufio.NewReader(strings.NewReader(rawRequest)))
		url, _ := url.Parse(URL)
		jar, _ := cookiejar.New(nil)
		jar.SetCookies(url, req.Cookies())
		client := http.Client{Jar: jar}
		request, _ := http.NewRequest("GET", URL, nil)
		request.Header.Set("user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:88.0) Gecko/20100101 Firefox/88.0")
		r, err := client.Do(request)
		check(err)
		defer r.Body.Close()

		body, _ := io.ReadAll(r.Body)
		check(os.WriteFile(fileName, body, 0644))
		logf("抓取 %d %s", r.StatusCode, id)
		time.Sleep(200 * time.Millisecond)
		return body, nil
	}
	body, err := os.ReadFile(fileName)
	if strings.Contains(string(body), "errcode") {
		os.Remove(fileName)
		panic(fmt.Errorf("缓存 %s 含错误响应（Cookie 失效/限流），已删除，请重试", id))
	}
	return body, err
}

func getBestBookMarks(root string, bookInfo BookInfo, booklistName string) bestBookMarkItem {
	bookId := bookInfo.BookId

	f, err := os.Create(filepath.Join(root, "books", booklistName, safeFileName(bookInfo.Title)+".md"))
	check(err)
	defer f.Close()

	fmt.Fprintln(f, "## "+bookInfo.Title)
	fmt.Fprintln(f, "\n"+bookInfo.Author, " - ", bookInfo.Category)
	fmt.Fprintln(f, "\n> "+bookInfo.Intro)

	result, err := getAndSaveResponse(root, "bookmark/"+bookId, "https://i.weread.qq.com/book/bestbookmarks?count=5000&bookId="+bookId)
	check(err)

	var marks bestBookMarks
	json.Unmarshal([]byte(result), &marks)

	sort.Slice(marks.Chapters, func(i, j int) bool {
		return marks.Chapters[i].Chapteruid < marks.Chapters[j].Chapteruid
	})
	sort.Slice(marks.Items, func(i, j int) bool {
		range1, _ := strconv.Atoi(strings.Split(marks.Items[i].Range, "-")[0])
		range2, _ := strconv.Atoi(strings.Split(marks.Items[j].Range, "-")[0])
		return range1 < range2
	})

	if len(marks.Items) > 0 {
		bestMark := marks.Items[0]
		for _, v := range marks.Chapters {
			fmt.Fprintln(f, "\n###", sanitizeText(v.Title))
			for i := 0; i < len(marks.Items); i++ {
				if marks.Items[i].Chapteruid == v.Chapteruid {
					fmt.Fprintln(f, "\n"+sanitizeText(marks.Items[i].Marktext))
				}
				if marks.Items[i].Totalcount > bestMark.Totalcount {
					bestMark = marks.Items[i]
				}
			}
		}
		if bestMark.Totalcount > 10 {
			return bestMark
		}
	}
	return bestBookMarkItem{}
}
