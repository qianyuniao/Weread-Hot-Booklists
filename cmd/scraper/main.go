// weread-scraper：命令行抓取微信读书热门书单，生成 INDEX.md 与 books/ 笔记。
// 核心逻辑在 scraper 包（同样被 weread-server 的 /api/update 复用）；
// 必须在项目根目录运行（读写 temp/、books/、cookie.txt、INDEX.md 等相对路径）。
package main

import (
	"fmt"
	"os"

	"github.com/qianyuniao/Weread-Hot-Booklists/scraper"
)

func main() {
	if err := scraper.Run("."); err != nil {
		fmt.Println("抓取失败:", err)
		os.Exit(1)
	}
	st := scraper.Snapshot()
	fmt.Printf("完成：书单 %d/%d，书籍 %d 本\n", st.ListsDone, st.ListsTotal, st.BooksDone)
}
