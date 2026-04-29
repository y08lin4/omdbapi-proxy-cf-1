package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func main() {
	r := bufio.NewReader(os.Stdin)
	fmt.Println("OMDb API Proxy 并发压测启动器")
	fmt.Println("直接回车会使用默认值。")
	fmt.Println()

	key := prompt(r, "CLIENT_KEY", os.Getenv("OMDB_CLIENT_KEY"))
	if strings.TrimSpace(key) == "" {
		fmt.Fprintln(os.Stderr, "错误：CLIENT_KEY 不能为空")
		os.Exit(2)
	}
	base := prompt(r, "API Base", "https://omdbapi.ailinyu.de")
	total := promptInt(r, "总请求数 -n", 100)
	concurrency := promptInt(r, "并发数 -c", 10)
	mode := promptChoice(r, "请求模式", "title", []string{"title", "search", "id", "poster", "custom"})
	q := "Inception"
	id := "tt1375666"
	path := "/?t=Inception"
	plot := "short"
	if mode == "title" || mode == "search" {
		q = prompt(r, "标题/关键词 -q", q)
	}
	if mode == "id" || mode == "poster" {
		id = prompt(r, "IMDb ID", id)
	}
	if mode == "title" || mode == "id" {
		plot = prompt(r, "plot 参数 short/full/空", plot)
	}
	if mode == "custom" {
		path = prompt(r, "自定义路径，不要带 apikey", path)
	}
	timeout := prompt(r, "单请求超时", "15s")
	jsonOut := promptBool(r, "是否输出 JSON 汇总", false)
	headerKey := promptBool(r, "是否用 X-API-Key 请求头传 key", false)

	args := []string{"run", ".\\tools\\loadtest.go",
		"-base", base,
		"-key", key,
		"-n", strconv.Itoa(total),
		"-c", strconv.Itoa(concurrency),
		"-mode", mode,
		"-timeout", timeout,
	}
	if mode == "title" || mode == "search" {
		args = append(args, "-q", q)
	}
	if mode == "id" || mode == "poster" {
		args = append(args, "-id", id)
	}
	if mode == "title" || mode == "id" {
		args = append(args, "-plot", plot)
	}
	if mode == "custom" {
		args = append(args, "-path", path)
	}
	if jsonOut {
		args = append(args, "-json")
	}
	if headerKey {
		args = append(args, "-header-key")
	}

	fmt.Println()
	fmt.Println("即将执行：")
	fmt.Println(maskCommand("go "+strings.Join(args, " "), key))
	if !promptBool(r, "确认开始", true) {
		fmt.Println("已取消")
		return
	}

	cmd := exec.Command("go", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "执行失败：", err)
		os.Exit(1)
	}
}

func prompt(r *bufio.Reader, label, def string) string {
	if def == "" {
		fmt.Printf("%s: ", label)
	} else {
		fmt.Printf("%s [%s]: ", label, def)
	}
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func promptInt(r *bufio.Reader, label string, def int) int {
	for {
		v := prompt(r, label, strconv.Itoa(def))
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			return n
		}
		fmt.Println("请输入大于 0 的整数。")
	}
}

func promptBool(r *bufio.Reader, label string, def bool) bool {
	defText := "N"
	if def {
		defText = "Y"
	}
	for {
		v := strings.ToLower(prompt(r, label+" Y/N", defText))
		switch v {
		case "y", "yes", "1", "true", "是":
			return true
		case "n", "no", "0", "false", "否":
			return false
		}
		fmt.Println("请输入 Y 或 N。")
	}
}

func promptChoice(r *bufio.Reader, label, def string, choices []string) string {
	set := map[string]bool{}
	for _, c := range choices {
		set[c] = true
	}
	for {
		v := prompt(r, label+" "+strings.Join(choices, "/"), def)
		if set[v] {
			return v
		}
		fmt.Println("可选值：" + strings.Join(choices, ", "))
	}
}

func maskCommand(s, key string) string {
	if key == "" {
		return s
	}
	masked := key
	if len(key) > 4 {
		masked = key[:2] + strings.Repeat("*", len(key)-4) + key[len(key)-2:]
	}
	return strings.ReplaceAll(s, key, masked)
}
