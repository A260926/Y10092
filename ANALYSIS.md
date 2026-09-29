# go-fsimpl 架构分析：FSMux 分派、URL 消费点与可选能力透传

本文基于当前提交（`f0ddb8a`）的源码逐层追踪，回答：不同 provider 在 `io/fs` 抽象下共享了什么、哪些能力只靠运行时接口断言透传。所有行号均可在工作区直接定位。

## 1. 分派模型：所有 provider 共享的只有"scheme → 构造函数"映射

`FSMux` 本身只是 `map[string]func(*url.URL) (fs.FS, error)`：

- 文件: mux.go:17 — `type FSMux map[string]func(*url.URL) (fs.FS, error)`
- 文件: mux.go:28 — `Add` 遍历 provider 的 `Schemes()`，把每个 scheme 映射到同一个 `fs.New`
- 文件: mux.go:30 — `m[scheme] = fs.New`，重复 scheme 直接覆盖（见风险 1）
- 文件: mux.go:36 — `Lookup(u string)` 先 `url.Parse`，再委托 `m.New(base)`（mux.go:42）
- 文件: mux.go:58 — `New` 用 `u.Scheme` 查 map（mux.go:59），命中后调用 `f(u)`（mux.go:64），**整个 `*url.URL` 原样下传**

也就是说，mux 层只消费 scheme；host、path、query、fragment 的语义完全由各 provider 自行解释。这是所有 provider 共享的唯一契约：`FSProvider` 接口（mux.go:67-74）+ 返回 `fs.FS`。

## 2. 四条真实调用链

### 2.1 `file:///etc/hostname` → `Open("hostname")`

1. 文件: mux.go:37 — `url.Parse` 得到 `Scheme=file, Host="", Path=/etc`
2. 文件: mux.go:59 — map 命中 `"file"`，该注册来自 `filefs.FS`
3. 文件: filefs/file.go:57 — `var FS = fsimpl.FSProviderFunc(New, "file")`
4. 文件: filefs/file.go:21 — `filefs.New(u)` 调 `pathForDirFS(u)`
5. 文件: filefs/file.go:34 — `rootPath = u.Path`（已解码）；filefs/file.go:42-48 处理 host：空 host 不加前缀，`"."` 变 `//./`，其余 host 拼成 UNC `//host/path`
6. 文件: filefs/file.go:24 — 返回 `&fileFS{root: os.DirFS(rootPath)}`
7. 文件: filefs/file.go:68 — `Open(name)` 直接转发 `f.root.Open(name)`，子路径在 `os.DirFS` 层与 root 拼接

注意 filefs 在 `New` 时一次性消费掉整个 path 作为根目录，之后的 `Open` 只见相对名。

### 2.2 `http://example.com/a/` → `Open("b.txt")`

1. mux 层同上，命中 `"http"`（https 复用同一构造函数）
2. 文件: httpfs/http.go:44 — `var FS = fsimpl.FSProviderFunc(New, "http", "https")`
3. 文件: httpfs/http.go:32 — `httpfs.New` **不拆分 URL**，把完整的 `u`（含 host、path、query）存为 `base`（httpfs/http.go:36）
4. 文件: httpfs/http.go:100 — `Open(name)` 先 `fs.ValidPath(name)` 校验相对名
5. 文件: httpfs/http.go:109 — `internal.SubURL(f.base, name)` 拼接 URL
6. 文件: internal/url.go:6-11 — `url.Parse(name)` + `base.ResolveReference(rel)` 做 RFC 3986 相对引用解析；internal/url.go:14-22 再合并 base 与相对引用的 query
7. 文件: httpfs/http.go:114 — 返回惰性 `httpFile`；真正发请求在 `request(GET)`（httpfs/http.go:163），用 `f.u.String()` 构造请求

与 filefs 相反，httpfs 把 path 的拼接推迟到每次 `Open`，靠 `ResolveReference` 完成。

### 2.3 `s3://mybucket/logs/` → `ReadFile("2026/09.txt")`（cloud blob 代表）

1. 文件: blobfs/blob.go:79 — `FSProviderFunc(New, s3blob.Scheme, gcsblob.Scheme, azureblob.Scheme)`，一个构造函数服务三种 scheme
2. 文件: blobfs/blob.go:51 — `New` 内对 scheme 做二次白名单校验（防御 mux 之外的直接调用）
3. 文件: blobfs/blob.go:57 — `root = strings.TrimPrefix(u.Path, "/")`，path 变成 key 前缀
4. 文件: blobfs/blob.go:158 — `Open`/`ReadFile`（blobfs/blob.go:199）首次调用时惰性 `openBucket`
5. 文件: blobfs/blob.go:127 — `openBucket` → `newOpener`（blobfs/blob.go:217，按 scheme 选 opener）→ `cleanCdkURL`
6. 文件: blobfs/s3blob.go:11 — `cleanS3URL` 白名单过滤 query 参数（region、endpoint、anonymous 等），未知参数被丢弃
7. 文件: blobfs/s3opener.go:137 — `s3blob.OpenBucketV2(ctx, client, u.Host, ...)`：**host 在这里才被消费，作为 bucket 名**
8. 文件: blobfs/blob.go:176 — 每个文件的 key 前缀为 `path.Join(f.root, path.Dir(name))`

### 2.4 `gcp+meta:///instance` → `ReadDir("instance")`（metadata 代表）

1. 文件: gcpmetafs/gcpmetafs.go:79 — 注册 scheme `"gcp+meta"`
2. 文件: gcpmetafs/gcpmetafs.go:62 — `New` 校验 scheme（gcpmetafs/gcpmetafs.go:63），`root = TrimPrefix(u.Path, "/")`（gcpmetafs/gcpmetafs.go:67）；host 完全忽略
3. 文件: gcpmetafs/gcpmetafs.go:166 — `ReadDir(name)` 用 `internal.ValidPath` 校验（比 `fs.ValidPath` 多拒绝反斜杠，见 internal/path.go:8-14）
4. 文件: gcpmetafs/gcpmetafs.go:171 — `getClient()` 惰性创建 metadata client（gcpmetafs/gcpmetafs.go:118）
5. 文件: gcpmetafs/gcpmetafs.go:523 — `gcpmetaFile.ReadDir` 调 `list()`，把 metadata 服务的层级条目映射成 `fs.DirEntry`

## 3. URL 各成分的消费点汇总

- **scheme**：唯一被 mux 消费的部分（文件: mux.go:59）；blobfs/gcpmetafs 在 provider 内二次校验（文件: blobfs/blob.go:51，文件: gcpmetafs/gcpmetafs.go:63）
- **host**：filefs 解释为 UNC 主机（文件: filefs/file.go:42）；blobfs 解释为 bucket 名（文件: blobfs/s3opener.go:137）；httpfs 留在 base URL 中随请求发出（文件: httpfs/http.go:36）；gcpmetafs 忽略
- **path**：filefs 在 `New` 时整体作为根目录（文件: filefs/file.go:34）；blobfs/gcpmetafs 去掉前导斜杠作为 key/路径前缀（文件: blobfs/blob.go:57，文件: gcpmetafs/gcpmetafs.go:67）；httpfs 保留在 base 中按相对引用逐次拼接（文件: internal/url.go:11）
- **双斜杠**：`scheme:///p` 的空 authority 对 filefs 意味着"本地路径、不加 UNC 前缀"（文件: filefs/file.go:45）；gitfs 额外用 path 中的 `//` 分隔仓库路径与仓内子路径（文件: gitfs/git.go:165）
- **fragment**：全库只有 gitfs 消费——`#refs/heads/x` 或 `#branch` 映射为 git 引用（文件: gitfs/git.go:171-179），clone 前清掉（文件: gitfs/git.go:198）；其余 provider 收到后完全不读，静默丢弃

## 4. WrappedFSProvider 何时调用 fs.Sub

- 文件: mux.go:97 — `WrappedFSProvider(fsys, schemes...)` 把任意 `fs.FS` 包装成 provider
- 文件: mux.go:100-104 — 取 `u.Path`：`"/"` 改写为 `"."`，否则 `TrimLeft(dir, "/")` 去掉全部前导斜杠
- 文件: mux.go:107 — **无条件**调用 `fs.Sub(fsys, dir)`

关键点：根路径并非"跳过 fs.Sub"，而是 `io/fs.Sub` 对 `"."` 特判、原样返回传入对象（标准库行为），所以 `foo:///` 与底层 FS 是同一对象（mux_test.go:66 的 `assert.Same` 可证）；任何非根路径都会得到一个新的子文件系统。若底层 FS 实现了 `fs.SubFS`（如 `fstest.MapFS`、`fileFS`，文件: filefs/file.go:88），`fs.Sub` 委托给它；否则回退到标准库通用包装器 `*fs.subFS`——后者只暴露标准接口，是风险 3 的根源。

## 5. 三种 With 扩展：为什么能力缺失时返回原对象

- 文件: extensions.go:17 — `WithContextFS`：类型断言 `internal.WithContexter`（文件: internal/types.go:85），命中则调 `WithContext`，**未命中原样返回 `fsys`**（extensions.go:22）
- 文件: extensions.go:27 — `WithHeaderFS` 同理，断言 `internal.WithHeaderer`（文件: internal/types.go:95）
- 文件: extensions.go:37 — `WithHTTPClientFS` 同理，断言 `internal.WithHTTPClienter`（文件: internal/types.go:90）

返回原对象是有意的 best-effort 装饰器设计：`fs.FS` 接口本身不含这些能力，若改成报错，调用方就无法对"可能不支持"的 provider（如 filefs 之于 HTTP header）做无条件包裹，组合性被破坏。代价是配置错误（把 header 注入 filefs）静默无效。注意命中时各 provider 返回的是**副本**而非原对象，例如：

- 文件: httpfs/http.go:59-68 — `WithContext` 复制 `httpFS` 结构体后替换 `ctx`
- 文件: blobfs/blob.go:94-103 — `WithContext` 同样返回拷贝

因此"身份"语义是双向的：未命中 → 同一对象；命中 → 新对象。调用方不能用指针比较判断是否生效。

## 6. 三个可复现风险

### 风险 1：scheme 重复注册静默覆盖

- **输入**：`m.Add(providerA)`（`Schemes() = ["mem"]`）后再 `m.Add(providerB)`（`Schemes() = ["mem"]`）
- **经过**：`FSMux.Add` 的循环赋值（文件: mux.go:28-32），map 键 `m["mem"]` 被第二次 `fs.New` 覆盖（文件: mux.go:30）
- **当前结果**：无错误、无日志；`m.Lookup("mem:///x")` 永远走到 providerB
- **业务影响**：应用与第三方库若都注册同名 scheme（如自定义 `s3` 与 blobfs 的 `s3`），行为取决于 `Add` 调用顺序；配置写错时表现为"读到了错误的后端"，极难排查
- **定性**：**规范允许**——`Add` 的文档注释明确写了 "they will be overridden"（mux.go:26-27）；潜在缺陷在于没有任何冲突检测或告警

### 风险 2：编码斜杠与非根路径的子目录边界

- **输入**：`mem:///root%2Fsub`（或 `file:///a%2Fb`、`mem:///root/../secret`）
- **经过**：`url.Parse` 在 mux 层就把 `%2F` 解码进 `u.Path`；`WrappedFSProvider` 直接用解码后的 `u.Path`（文件: mux.go:100）算出 `dir="root/sub"` 交给 `fs.Sub`（文件: mux.go:107）；filefs 同样用解码后的 `u.Path` 作根（文件: filefs/file.go:34）
- **当前结果**：单个 URL 段 `root%2Fsub` 实际穿越了两层目录，`fs.ReadFile(sub, "file.txt")` 成功读到 `root/sub/file.txt`；`..` 段会被 `fs.Sub` 的 `ValidPath` 拒绝（报错 `sub root/../rootfile.txt: invalid argument`），但 filefs 路径上的 `os.DirFS` 不防御符号链接逃逸（`io/fs.Sub` 文档亦明确不作 chroot 保证）
- **业务影响**：若上层把 URL 段当作多租户/权限边界（"第 N 段是租户目录"），编码斜杠可单段越界；`WrappedFSProvider` 的非根路径看似有边界，实际边界在解码后的完整路径上
- **定性**：百分号解码是 RFC 3986 与 `net/url` 的**规范行为**；潜在缺陷是 filefs/WrappedFSProvider 均未在解码后重新校验段边界，且 `os.DirFS` 的 symlink 逃逸面被原样继承

### 风险 3：包装后可选接口身份丢失

- **输入**：对"实现了 `WithContext` 但未实现 `fs.SubFS`"的 FS 调 `fs.Sub(fsys, "sub")`，再对结果调 `WithContextFS(ctx, …)`；或对 filefs 调 `WithHeaderFS`
- **经过**：`fs.Sub` 回退到标准库 `*fs.subFS` 通用包装器，它只实现 `Open/ReadDir/ReadFile/Glob/Stat/Sub`，不转发 `WithContext`；`WithContextFS` 的类型断言失败（文件: extensions.go:18），走 `return fsys` 分支（文件: extensions.go:22）
- **当前结果**：扩展静默无效——`WithContext` 一次都没被调用，返回的是包装器本身；调用方拿不到任何错误信号。反过来，命中时返回新副本（文件: httpfs/http.go:65-67），指针身份改变
- **业务影响**：调用方以为注入了可取消的 context 或认证 header，实际请求用的是 `context.Background()` 和空 header；在长链路（mux → Sub → With*）中能力在哪一环丢失无法静态判断
- **定性**：返回原对象是**规范允许**的设计选择（见第 5 节）；潜在缺陷是缺少"能力是否生效"的反馈通道，且 `*fs.subFS` 不透明转发可选接口

## 7. 不变量的可执行证明

`understanding_proof_test.go`（纯内存 `fstest.MapFS` + 自定义 provider，不触网络/云/git/consul）证明了两条不变量：

- `TestUnderstandingProofMuxDispatchAndSubPath`：mux 只消费 scheme（host/path/query/fragment 原样到达 provider）；`WrappedFSProvider` 对根路径保持对象身份、对非根路径产生子 FS；`%2F` 在到达 provider 前已被解码；`..` 被 `fs.Sub` 拒绝
- `TestUnderstandingProofCapabilityIdentity`：`With*` 在能力缺失时返回同一对象、命中时返回新对象；通用 `fs.Sub` 包装器会隐藏可选接口，使后续扩展静默失效

运行：`go test -run 'TestUnderstandingProof' -count=1 -v .`
