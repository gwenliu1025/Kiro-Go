# Kiro-Go 改造前基线

日期：2026-07-14

## 代码基线

- 设计基线提交：`2ab979f`
- 实施分支：`feat/kiro-native-high-cache`
- 实施计划提交：`accf5d4`

## 已确认的既有测试契约冲突

改造前的 `go test ./...` 稳定失败于：

- `TestClaudeToolResultMixedTextAndImage`
- `TestOpenAIToolResultImageCarriedWhenFollowedByUser`

根因不是本次高缓存改造。提交 `72da572` 已要求孤立或历史工具结果扁平化，以避免 Kiro 上游拒绝历史结构化工具结果；提交 `2ad0c56` 新增的两个图片用例仍断言旧的结构化 `ToolResults` 形态。

定向修正测试后还暴露出一个真实基线缺陷：Claude 孤立工具结果同时携带文本和图片时，图片占位文本会抢先成为 `finalContent`，导致工具结果正文丢失。基线修复将扁平化后的工具正文追加到图片提示后，不恢复上游不接受的结构化历史工具结果。

基线修正包含测试契约更新和上述最小正文保留修复，继续验证：

- 图片保留在正确的当前消息或历史消息；
- 工具结果文本没有丢失；
- 历史工具结果已经扁平化；
- 工具图片不会泄漏到后续用户消息。

## 验证命令

```powershell
go test ./proxy -run '^(TestClaudeToolResultMixedTextAndImage|TestOpenAIToolResultImageCarriedWhenFollowedByUser)$' -count=1 -v
go test ./...
go build ./...
go vet ./...
```

## Race 环境限制

当前 Windows 环境默认 `CGO_ENABLED=0`，且未安装 `gcc`，因此本机不能执行有效的 `go test -race ./...`。最终竞态验证必须在 Linux CI 或毕业机执行，不能用普通测试替代。

## 旧双遍请求分析基线

命令：

```powershell
go test ./proxy -run '^$' -bench '^BenchmarkLegacyClaudeAnalysis' -benchmem -count=5
```

原始结果：

```text
goos: windows
goarch: amd64
pkg: kiro-go/proxy
cpu: Intel(R) Core(TM) Ultra 9 275HX
BenchmarkLegacyClaudeAnalysis1KB-24      	   30344	     35465 ns/op	  28.87 MB/s	   33327 B/op	     272 allocs/op
BenchmarkLegacyClaudeAnalysis1KB-24      	   32899	     36897 ns/op	  27.75 MB/s	   33329 B/op	     272 allocs/op
BenchmarkLegacyClaudeAnalysis1KB-24      	   32380	     37669 ns/op	  27.18 MB/s	   33329 B/op	     272 allocs/op
BenchmarkLegacyClaudeAnalysis1KB-24      	   29334	     40141 ns/op	  25.51 MB/s	   33331 B/op	     272 allocs/op
BenchmarkLegacyClaudeAnalysis1KB-24      	   35634	     36889 ns/op	  27.76 MB/s	   33328 B/op	     272 allocs/op
BenchmarkLegacyClaudeAnalysis64KB-24     	     958	   1555616 ns/op	  42.13 MB/s	 1213827 B/op	     279 allocs/op
BenchmarkLegacyClaudeAnalysis64KB-24     	    1090	   1360731 ns/op	  48.16 MB/s	 1207140 B/op	     279 allocs/op
BenchmarkLegacyClaudeAnalysis64KB-24     	     984	   1192433 ns/op	  54.96 MB/s	 1209779 B/op	     279 allocs/op
BenchmarkLegacyClaudeAnalysis64KB-24     	    1105	   1189598 ns/op	  55.09 MB/s	 1209938 B/op	     279 allocs/op
BenchmarkLegacyClaudeAnalysis64KB-24     	    1539	   1207571 ns/op	  54.27 MB/s	 1211168 B/op	     279 allocs/op
BenchmarkLegacyClaudeAnalysis512KB-24    	     136	   8481985 ns/op	  61.81 MB/s	13171773 B/op	     318 allocs/op
BenchmarkLegacyClaudeAnalysis512KB-24    	     126	   9589775 ns/op	  54.67 MB/s	13890100 B/op	     326 allocs/op
BenchmarkLegacyClaudeAnalysis512KB-24    	     126	   9997515 ns/op	  52.44 MB/s	13829382 B/op	     325 allocs/op
BenchmarkLegacyClaudeAnalysis512KB-24    	     133	   9604264 ns/op	  54.59 MB/s	13883938 B/op	     326 allocs/op
BenchmarkLegacyClaudeAnalysis512KB-24    	     139	   8591152 ns/op	  61.03 MB/s	13533012 B/op	     322 allocs/op
BenchmarkLegacyClaudeAnalysis2MB-24      	      49	  23552188 ns/op	  89.04 MB/s	53545299 B/op	     327 allocs/op
BenchmarkLegacyClaudeAnalysis2MB-24      	      49	  21934414 ns/op	  95.61 MB/s	52589533 B/op	     325 allocs/op
BenchmarkLegacyClaudeAnalysis2MB-24      	      62	  24280716 ns/op	  86.37 MB/s	51286265 B/op	     321 allocs/op
BenchmarkLegacyClaudeAnalysis2MB-24      	      74	  23509274 ns/op	  89.21 MB/s	52353225 B/op	     323 allocs/op
BenchmarkLegacyClaudeAnalysis2MB-24      	      44	  24135327 ns/op	  86.89 MB/s	54088376 B/op	     330 allocs/op
```

## 即时假上游首字基线

基准通过 `httptest.Server` 接收完整 Kiro 请求并立即返回最小 `assistantResponseEvent`。`first-token-ns/op` 从进入 Claude 消息处理器开始，截止 `message_start` 或首个内容事件写入测试响应器；普通 `ns/op` 还包含假上游响应收尾。

命令：

```powershell
go test ./proxy -run '^$' -bench '^BenchmarkClaudeFirstToken' -benchmem -count=10
```

原始结果：

```text
goos: windows
goarch: amd64
pkg: kiro-go/proxy
cpu: Intel(R) Core(TM) Ultra 9 275HX
BenchmarkClaudeFirstToken1KB-24      	    4974	    245806 ns/op	   4.17 MB/s	    227941 first-token-ns/op	  111217 B/op	     722 allocs/op
BenchmarkClaudeFirstToken1KB-24      	    5481	    224613 ns/op	   4.56 MB/s	    210586 first-token-ns/op	  111609 B/op	     722 allocs/op
BenchmarkClaudeFirstToken1KB-24      	    5607	    219504 ns/op	   4.67 MB/s	    202885 first-token-ns/op	  111311 B/op	     722 allocs/op
BenchmarkClaudeFirstToken1KB-24      	    5368	    215592 ns/op	   4.75 MB/s	    204733 first-token-ns/op	  111317 B/op	     722 allocs/op
BenchmarkClaudeFirstToken1KB-24      	    4785	    265771 ns/op	   3.85 MB/s	    249212 first-token-ns/op	  114213 B/op	     723 allocs/op
BenchmarkClaudeFirstToken1KB-24      	    4554	    224183 ns/op	   4.57 MB/s	    210394 first-token-ns/op	  112767 B/op	     722 allocs/op
BenchmarkClaudeFirstToken1KB-24      	    5329	    237273 ns/op	   4.32 MB/s	    217851 first-token-ns/op	  113131 B/op	     723 allocs/op
BenchmarkClaudeFirstToken1KB-24      	    6135	    228845 ns/op	   4.47 MB/s	    212390 first-token-ns/op	  114097 B/op	     723 allocs/op
BenchmarkClaudeFirstToken1KB-24      	    4954	    250232 ns/op	   4.09 MB/s	    233566 first-token-ns/op	  113680 B/op	     723 allocs/op
BenchmarkClaudeFirstToken1KB-24      	    5095	    225248 ns/op	   4.55 MB/s	    213161 first-token-ns/op	  113192 B/op	     723 allocs/op
BenchmarkClaudeFirstToken64KB-24     	     374	   3015174 ns/op	  21.74 MB/s	   2998052 first-token-ns/op	 2843027 B/op	     807 allocs/op
BenchmarkClaudeFirstToken64KB-24     	     362	   2964157 ns/op	  22.11 MB/s	   2905249 first-token-ns/op	 2866579 B/op	     809 allocs/op
BenchmarkClaudeFirstToken64KB-24     	     378	   3349919 ns/op	  19.56 MB/s	   3307799 first-token-ns/op	 2870525 B/op	     810 allocs/op
BenchmarkClaudeFirstToken64KB-24     	     402	   2940574 ns/op	  22.29 MB/s	   2914301 first-token-ns/op	 2857098 B/op	     808 allocs/op
BenchmarkClaudeFirstToken64KB-24     	     374	   2778255 ns/op	  23.59 MB/s	   2755890 first-token-ns/op	 2875972 B/op	     810 allocs/op
BenchmarkClaudeFirstToken64KB-24     	     412	   2837722 ns/op	  23.09 MB/s	   2804405 first-token-ns/op	 2856535 B/op	     808 allocs/op
BenchmarkClaudeFirstToken64KB-24     	     430	   2761600 ns/op	  23.73 MB/s	   2740375 first-token-ns/op	 2825181 B/op	     806 allocs/op
BenchmarkClaudeFirstToken64KB-24     	     416	   2923010 ns/op	  22.42 MB/s	   2906065 first-token-ns/op	 2868015 B/op	     809 allocs/op
BenchmarkClaudeFirstToken64KB-24     	     432	   2879343 ns/op	  22.76 MB/s	   2828598 first-token-ns/op	 2838740 B/op	     807 allocs/op
BenchmarkClaudeFirstToken64KB-24     	     406	   3054374 ns/op	  21.46 MB/s	   2999032 first-token-ns/op	 2875303 B/op	     810 allocs/op
BenchmarkClaudeFirstToken512KB-24    	      69	  16037743 ns/op	  32.69 MB/s	  16020281 first-token-ns/op	26886974 B/op	     889 allocs/op
BenchmarkClaudeFirstToken512KB-24    	      94	  15449829 ns/op	  33.93 MB/s	  15402324 first-token-ns/op	26020134 B/op	     881 allocs/op
BenchmarkClaudeFirstToken512KB-24    	      75	  16592227 ns/op	  31.60 MB/s	  16580912 first-token-ns/op	25506004 B/op	     875 allocs/op
BenchmarkClaudeFirstToken512KB-24    	      91	  15781898 ns/op	  33.22 MB/s	  15741576 first-token-ns/op	26246604 B/op	     881 allocs/op
BenchmarkClaudeFirstToken512KB-24    	      75	  16873520 ns/op	  31.07 MB/s	  16858872 first-token-ns/op	25860452 B/op	     877 allocs/op
BenchmarkClaudeFirstToken512KB-24    	      87	  15809123 ns/op	  33.16 MB/s	  15781321 first-token-ns/op	26294733 B/op	     884 allocs/op
BenchmarkClaudeFirstToken512KB-24    	      67	  15583710 ns/op	  33.64 MB/s	  15557781 first-token-ns/op	26168813 B/op	     881 allocs/op
BenchmarkClaudeFirstToken512KB-24    	      61	  17448246 ns/op	  30.05 MB/s	  17444648 first-token-ns/op	26672157 B/op	     888 allocs/op
BenchmarkClaudeFirstToken512KB-24    	      61	  19446167 ns/op	  26.96 MB/s	  19430985 first-token-ns/op	25549049 B/op	     876 allocs/op
BenchmarkClaudeFirstToken512KB-24    	      92	  15666389 ns/op	  33.47 MB/s	  15618663 first-token-ns/op	26680396 B/op	     886 allocs/op
BenchmarkClaudeFirstToken2MB-24      	      25	  49437076 ns/op	  42.42 MB/s	  49370168 first-token-ns/op	100749670 B/op	     937 allocs/op
BenchmarkClaudeFirstToken2MB-24      	      25	  62327596 ns/op	  33.65 MB/s	  62265988 first-token-ns/op	102196528 B/op	     939 allocs/op
BenchmarkClaudeFirstToken2MB-24      	      25	  46270736 ns/op	  45.32 MB/s	  46198728 first-token-ns/op	101176120 B/op	     935 allocs/op
BenchmarkClaudeFirstToken2MB-24      	      21	  56425086 ns/op	  37.17 MB/s	  56441852 first-token-ns/op	103653979 B/op	     950 allocs/op
BenchmarkClaudeFirstToken2MB-24      	      21	  52423890 ns/op	  40.00 MB/s	  52339962 first-token-ns/op	99466042 B/op	     934 allocs/op
BenchmarkClaudeFirstToken2MB-24      	      21	  53930590 ns/op	  38.89 MB/s	  53900657 first-token-ns/op	105641033 B/op	     948 allocs/op
BenchmarkClaudeFirstToken2MB-24      	      26	  56958119 ns/op	  36.82 MB/s	  56948515 first-token-ns/op	103807195 B/op	     946 allocs/op
BenchmarkClaudeFirstToken2MB-24      	      21	  49307952 ns/op	  42.53 MB/s	  49302938 first-token-ns/op	102673111 B/op	     942 allocs/op
BenchmarkClaudeFirstToken2MB-24      	      26	  51964350 ns/op	  40.36 MB/s	  51941569 first-token-ns/op	104024837 B/op	     945 allocs/op
BenchmarkClaudeFirstToken2MB-24      	      16	  63267838 ns/op	  33.15 MB/s	  63083944 first-token-ns/op	96880457 B/op	     926 allocs/op
```

## 新单遍请求分析结果

命令：

```powershell
go test ./proxy -run '^$' -bench '^BenchmarkNewClaudeAnalysis' -benchmem -count=5
```

原始结果：

```text
goos: windows
goarch: amd64
pkg: kiro-go/proxy
cpu: Intel(R) Core(TM) Ultra 9 275HX
BenchmarkNewClaudeAnalysis1KB-24      	   82813	     14543 ns/op	  70.41 MB/s	    3648 B/op	     280 allocs/op
BenchmarkNewClaudeAnalysis1KB-24      	   89988	     12972 ns/op	  78.94 MB/s	    3648 B/op	     280 allocs/op
BenchmarkNewClaudeAnalysis1KB-24      	   91908	     12994 ns/op	  78.81 MB/s	    3648 B/op	     280 allocs/op
BenchmarkNewClaudeAnalysis1KB-24      	   86229	     13253 ns/op	  77.27 MB/s	    3648 B/op	     280 allocs/op
BenchmarkNewClaudeAnalysis1KB-24      	   96470	     16746 ns/op	  61.15 MB/s	    3648 B/op	     280 allocs/op
BenchmarkNewClaudeAnalysis64KB-24     	    3051	    484430 ns/op	 135.28 MB/s	   34704 B/op	     133 allocs/op
BenchmarkNewClaudeAnalysis64KB-24     	    4040	    322408 ns/op	 203.27 MB/s	   34705 B/op	     133 allocs/op
BenchmarkNewClaudeAnalysis64KB-24     	    3930	    433621 ns/op	 151.14 MB/s	   34704 B/op	     133 allocs/op
BenchmarkNewClaudeAnalysis64KB-24     	    3566	    307150 ns/op	 213.37 MB/s	   34704 B/op	     133 allocs/op
BenchmarkNewClaudeAnalysis64KB-24     	    3974	    438117 ns/op	 149.59 MB/s	   34705 B/op	     133 allocs/op
BenchmarkNewClaudeAnalysis512KB-24    	     496	   2604742 ns/op	 201.28 MB/s	   34704 B/op	     133 allocs/op
BenchmarkNewClaudeAnalysis512KB-24    	     331	   3373963 ns/op	 155.39 MB/s	   34704 B/op	     133 allocs/op
BenchmarkNewClaudeAnalysis512KB-24    	     476	   2579555 ns/op	 203.25 MB/s	   34704 B/op	     133 allocs/op
BenchmarkNewClaudeAnalysis512KB-24    	     464	   2723339 ns/op	 192.52 MB/s	   34706 B/op	     133 allocs/op
BenchmarkNewClaudeAnalysis512KB-24    	     471	   3484929 ns/op	 150.44 MB/s	   34704 B/op	     133 allocs/op
BenchmarkNewClaudeAnalysis2MB-24      	     121	  11120081 ns/op	 188.59 MB/s	   34704 B/op	     133 allocs/op
BenchmarkNewClaudeAnalysis2MB-24      	     120	  12168232 ns/op	 172.35 MB/s	   34704 B/op	     133 allocs/op
BenchmarkNewClaudeAnalysis2MB-24      	     100	  11453239 ns/op	 183.11 MB/s	   34704 B/op	     133 allocs/op
BenchmarkNewClaudeAnalysis2MB-24      	     118	   9920244 ns/op	 211.40 MB/s	   34714 B/op	     133 allocs/op
BenchmarkNewClaudeAnalysis2MB-24      	     122	  10121364 ns/op	 207.20 MB/s	   34704 B/op	     133 allocs/op
```

与旧双遍相比，新分析器在四档输入上均减少运行时间；512 KB 和 2 MB 请求的每次分配量稳定在约 34 KB，不再随正文大小线性放大。
