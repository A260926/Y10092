# go-fsimpl 分派与可选能力分析

本分析针对当前提交 `f0ddb8a`。所有结论均以源码文件与行号为准（引用格式：`路径:行号`），不引用 README。配套的可复现实验在 `understanding_proof_test.go` 中，全部基于内存 `testing/fstest.MapFS` 与自定义 provider，不访问网络、云、git daemon 或 consul。

> 说明：工作区快照缺少上游的 `internal/env` 包，导致仓库初始状态 `go build ./...` 失败。为恢复“依赖齐全、可编译”的基线，补回了仅含 `Getenv/GetenvFS` 的 `internal/env/env.go`（blobfs/gitfs/vaultfs 都依赖 `env.GetenvFS`），未改动任何公开 API。

## 1. 各 provider 在同一抽象下共享了什么

- 统一入口只有两个接口：`FSProvider`（`Schemes()` + `New(*url.URL)`，`文件: mux.go:68` 的接口定义）和返回值的标准接口 `io/fs.FS`。`FSMux` 本身是 `map[string]func(*url.URL) (fs.FS, error)`，并且自身实现了 `FSProvider`（`文件: mux.go:18`）。
- 分派只看 `url.URL.Scheme`：`FSMux.New` 用 `m[u.Scheme]` 取构造函数（`文件: mux.go:59`），未注册返回 `no filesystem registered for scheme`（`文件: mux.go:61`）。
- 每个 provider 通过包级变量 `FS = fsimpl.FSProviderFunc(New, ...schemes)` 注册自己负责的 scheme（`文件: filefs/file.go:57`、`文件: httpfs/http.go:44`、`文件: blobfs/blob.go:79`、`文件: gcpmetafs/gcpmetafs.go:79`、`文件: gitfs/git.go:69`）。
- 超出 `io/fs.FS` 的能力（context、HTTP header、HTTP client、IMDS、content-type 等）不进接口签名，而是靠运行时类型断言透传：三个公共 helper 在 `文件: extensions.go:17`、`文件: extensions.go:27`、`文件: extensions.go:37`，断言的内部接口定义在 `文件: internal/types.go:85`、`文件: internal/types.go:90`、`文件: internal/types.go:95`。
- 路径校验在多数 provider 里复用同一个内部助手 `internal.ValidPath`（`文件: internal/path.go:8`），它在标准 `fs.ValidPath` 之外额外拒绝反斜杠；`blobFS.Sub`（`文件: blobfs/blob.go:143`）与 `gcpmetaFS.Sub`（`文件: gcpmetafs/gcpmetafs.go:128`）都先用它校验。
- “子目录”有两种实现风格：
  - 通用包装：`WrappedFSProvider` 在 `New` 时消费 `u.Path` 并调用标准库 `fs.Sub`（`文件: mux.go:100` 到 `文件: mux.go:107`）。
  - provider 自持 `root` 字段：blob/gcpmeta 在 `New` 时 `strings.TrimPrefix(u.Path, "/")` 得到前缀（`文件: blobfs/blob.go:57`、`文件: gcpmetafs/gcpmetafs.go:67`），在 `Sub` 里用 `path.Join(root, name)` 加深前缀（`文件: blobfs/blob.go:153`、`文件: gcpmetafs/gcpmetafs.go:138`），不真正做 chroot，最终对象 key / 元数据路径在 `Open`/`Read` 时再拼接（`文件: blobfs/blob.go:176`、`文件: blobfs/blob.go:213`、`文件: gcpmetafs/gcpmetafs.go:153`、`文件: gcpmetafs/gcpmetafs.go:286`）。

## 2. 四条真实调用链（FSMux.Lookup → Open/ReadDir）

### 2.1 `file://` —— scheme/host/path 在构造期一次性消费

以 `file://host/share/foo`（或本机 `file:///tmp/foo`）为例：

1. `FSMux.Lookup` 调 `url.Parse`（`文件: mux.go:37`），随后 `m.New(base)`（`文件: mux.go:42`）。
2. `FSMux.New` 用 `u.Scheme == "file"` 命中 `filefs.New`（`文件: mux.go:59` → `文件: filefs/file.go:57`）。
3. `filefs.New` 调 `pathForDirFS(u)` 把 URL 映射成 OS 路径，再 `os.DirFS(rootPath)`（`文件: filefs/file.go:21` 到 `文件: filefs/file.go:24`）。
   - `u.Path` 在这里被消费：空路径返回 `""`（`文件: filefs/file.go:30`）；Windows 盘符剥掉前导 `/`（`文件: filefs/file.go:36`）。
   - `u.Host` 在这里被消费：host 为空什么都不做，host 为 `"."` 拼成 `//./...`，其他 host 拼成 UNC `//host/path`（`文件: filefs/file.go:42` 到 `文件: filefs/file.go:49`）。
4. 之后 `fs.FS.Open(name)` 直接委托给 `os.DirFS` 的根（`文件: filefs/file.go:68` 到 `文件: filefs/file.go:70`）；`ReadDir` 走 `fs.ReadDir(f.root, name)`（`文件: filefs/file.go:76` 到 `文件: filefs/file.go:78`）。
5. `fileFS.Sub` 用标准库 `fs.Sub` 再下移（`文件: filefs/file.go:88` 到 `文件: filefs/file.go:89`）。

消费点总结：scheme 在 mux（`文件: mux.go:59`）；host 与 path 仅在 `pathForDirFS`（`文件: filefs/file.go:29`）；双斜杠 `file://` 只是标准 URL 的 authority 分隔符，host 为空时等同于 `file:/`；fragment 完全没人读。

### 2.2 `http://` —— 惰性、在 Open 时用 URL 引用解析拼路径

以 `http://example.com/data?token=x` 后 `Open("a/b.txt")` 为例：

1. `Lookup → New`，scheme `"http"`（或 `"https"`）命中 `httpfs.New`（注册见 `文件: httpfs/http.go:44`），构造函数只保存 `base: u`、默认 client/header（`文件: httpfs/http.go:32` 到 `文件: httpfs/http.go:38`）。此时 host/path/query 都不访问。
2. `httpFS.Open(name)` 先用标准 `fs.ValidPath` 拒绝非法名（`文件: httpfs/http.go:101`），再调 `internal.SubURL(f.base, name)`（`文件: httpfs/http.go:109`）。
3. `SubURL` 把 name 当相对引用 `url.Parse(name)` 后 `base.ResolveReference(rel)`（`文件: internal/url.go:6`、`文件: internal/url.go:11`），并合并 query（`文件: internal/url.go:14` 到 `文件: internal/url.go:22`）。host 在此解析结果中继承自 base（标准库 `net/url.ResolveReference`）。
4. `Open` 此时并不发请求，只返回 `httpFile`（`文件: httpfs/http.go:114` 到 `文件: httpfs/http.go:120`）。真正 I/O 在第一次 `Read` → `request(GET)`（`文件: httpfs/http.go:202` 到 `文件: httpfs/http.go:204`），由 `http.NewRequestWithContext(... f.u.String() ...)`（`文件: httpfs/http.go:164`）和 `client.Do`（`文件: httpfs/http.go:171`）发出；`Stat` 用 HEAD，405/401 时回退 GET（`文件: httpfs/http.go:215` 到 `文件: httpfs/http.go:231`）。
5. `httpFS.Sub` 同样靠 `SubURL` 改 base，返回新的 `httpFS`（`文件: httpfs/http.go:138` 到 `文件: httpfs/http.go:148`）。

消费点总结：scheme 在 mux；host 在最终请求 URL（继承 base）；path 在每次 `SubURL` 惰性拼接；fragment 在 `ResolveReference` 处理相对引用时一般被丢弃（相对引用无 fragment 时才继承，见标准库），业务请求 URL 不带 fragment；`httpFS` 没有实现 `ReadDir`，列目录只能靠外部 `fs.ReadDir` 对“以 `/` 结尾的对象”做 Open，这是接口能力差异。

### 2.3 cloud blob：`s3://` —— host 是桶名，path 是对象前缀

以 `s3://my-bucket/config/app.yaml?region=us-west-1` 后 `ReadFile("app.yaml")` 为例：

1. scheme `"s3"`（另有 `"gs"`、`"azblob"`）命中 `blobfs.New`（注册见 `文件: blobfs/blob.go:79`）；构造函数先白名单校验 scheme（`文件: blobfs/blob.go:51` 到 `文件: blobfs/blob.go:55`），再 `root := strings.TrimPrefix(u.Path, "/")`（`文件: blobfs/blob.go:57`），并创建一个内嵌的 `aws+imds` 文件系统备用（`文件: blobfs/blob.go:59` 到 `文件: blobfs/blob.go:63`）。host（桶名）此刻不校验。
2. 第一次 `Open`/`ReadFile` 时惰性 `openBucket()`（`文件: blobfs/blob.go:163`、`文件: blobfs/blob.go:204`）：`newOpener` 按 `f.base.Scheme` 选 S3/GCS/Azurite opener（`文件: blobfs/blob.go:128`、`文件: blobfs/blob.go:217`）；`cleanCdkURL` 剥掉 Go CDK 不认识的 query 参数（`文件: blobfs/blob.go:133`、`文件: blobfs/blob.go:250`，S3 白名单在 `文件: blobfs/s3blob.go:11` 起）。
3. S3 opener 最终用 `s3blob.OpenBucketV2(ctx, clientV2, u.Host, ...)` —— **host 在此被当成桶名**消费（`文件: blobfs/s3opener.go:137`）；region 缺失时还会从 IMDS 文件系统读 `meta-data/placement/region`（`文件: blobfs/s3opener.go:125` 到 `文件: blobfs/s3opener.go:133`）。
4. `ReadFile` 用 `path.Join(f.root, name)` 拼对象 key 再 `bucket.ReadAll`（`文件: blobfs/blob.go:213`）；`Open` 把目录名放进 `root`、文件名放进 `name`（`文件: blobfs/blob.go:174` 到 `文件: blobfs/blob.go:176`），`Read` 用 `bucket.NewReader(path.Join(root, name))`（`文件: blobfs/blob.go:316`），`Stat` 用 `Attributes`，NotFound 时用 `ListPage(Delimiter:"/")` 探测“目录”（`文件: blobfs/blob.go:332` 到 `文件: blobfs/blob.go:361`）。
5. `blobFS.Sub` 只加深前缀 `fsys.root = path.Join(fsys.root, name)`（`文件: blobfs/blob.go:153`），不重建桶。

消费点总结：scheme 在 mux 与 `newOpener`（`文件: blobfs/blob.go:217`）；host 是桶名（`文件: blobfs/s3opener.go:137`）；path 是对象 key 前缀（`文件: blobfs/blob.go:57`、`文件: blobfs/blob.go:153`）；query 是连接参数并在 `cleanS3URL` 被过滤（`文件: blobfs/s3blob.go:16` 到 `文件: blobfs/s3blob.go:41`）；fragment 无人消费；路径里没有双斜杠语义，`path.Join` 还会折叠多余斜杠。

### 2.4 metadata：`gcp+meta://` —— path 是元数据文档前缀，ReadDir 真实存在

以 `gcp+meta:///instance` 后 `ReadDir(".")` 为例：

1. scheme `"gcp+meta"` 命中 `gcpmetafs.New`（注册 `文件: gcpmetafs/gcpmetafs.go:79`），构造函数校验 scheme（`文件: gcpmetafs/gcpmetafs.go:63` 到 `文件: gcpmetafs/gcpmetafs.go:65`），`root := strings.TrimPrefix(u.Path, "/")`（`文件: gcpmetafs/gcpmetafs.go:67`）。该 scheme 是 opaque 友好名，但此处仍按分层 path 处理。
2. `ReadDir(name)` 先 `internal.ValidPath` 校验（`文件: gcpmetafs/gcpmetafs.go:167`），构造一个带 `DirInfo` 的 `gcpmetaFile`（`文件: gcpmetafs/gcpmetafs.go:173` 到 `文件: gcpmetafs/gcpmetafs.go:179`），再调其 `ReadDir(-1)`（`文件: gcpmetafs/gcpmetafs.go:181`）。
3. `gcpmetaFile.ReadDir` 惰性 `list()`（`文件: gcpmetafs/gcpmetafs.go:524` 附近），`list` 用 `path.Join(f.root, f.name)`（`文件: gcpmetafs/gcpmetafs.go:329`）分流到 `listRoot`（硬编码 instance/project，`文件: gcpmetafs/gcpmetafs.go:362`）、`listInstance`（`文件: gcpmetafs/gcpmetafs.go:384`）等；未知目录走 `fetchDirectoryListing`，把请求路径补尾斜杠后 `client.GetWithContext`（`文件: gcpmetafs/gcpmetafs.go:425` 到 `文件: gcpmetafs/gcpmetafs.go:440`）。
4. 普通 `Open(name)` 不立即请求，只记录 `name=path.Base(name)`、`root=path.Join(root, path.Dir(name))`（`文件: gcpmetafs/gcpmetafs.go:150` 到 `文件: gcpmetafs/gcpmetafs.go:153`）；读/Stat 时 `getValue` 用 `path.Join(root, name)` 形成完整元数据路径（`文件: gcpmetafs/gcpmetafs.go:285` 到 `文件: gcpmetafs/gcpmetafs.go:293`），404 映射成 `fs.ErrNotExist`（`文件: gcpmetafs/gcpmetafs.go:259` 到 `文件: gcpmetafs/gcpmetafs.go:268`）。
5. `Sub` 同样是前缀叠加（`文件: gcpmetafs/gcpmetafs.go:128` 到 `文件: gcpmetafs/gcpmetafs.go:140`）。

消费点总结：scheme 在 mux 与构造校验（`文件: gcpmetafs/gcpmetafs.go:63`）；host 被忽略（元数据服务地址固定，由 metadata client/HTTP client 决定）；path 是文档前缀；fragment 无人消费；没有双斜杠约定。对比另一条 metadata 链 `aws+imds`（`文件: awsimdsfs/awsimdsfs.go:55` 起）也是同样的“path 去前导斜杠当 root”模式。

### 2.5 附：双斜杠与 fragment 真正被消费的特例（git）

四类目标 scheme 都不消费双斜杠/fragment；该约定只在 git provider 出现，作为对照：

- 双斜杠：`splitRepoPath` 用 `strings.Cut(repopath, "//")` 把“仓库路径”和“仓库内子目录”切开（`文件: gitfs/git.go:163` 到 `文件: gitfs/git.go:167`），子目录在 clone 后经 `fs.Sub(fsys, validPath(f.root))` 生效（`文件: gitfs/git.go:128`）。scheme 上的 `git+` 前缀在 `文件: gitfs/git.go:46` 被剥掉。
- fragment：`refFromURL` 把 `#refs/...` 当完整 ref、其他 fragment 当分支名（`文件: gitfs/git.go:171` 到 `文件: gitfs/git.go:179`），发请求前再清空 `u.Fragment`/`u.RawQuery`（`文件: gitfs/git.go:198` 到 `文件: gitfs/git.go:199`）。

## 3. WrappedFSProvider 何时调用 fs.Sub

`WrappedFSProvider` 是把“一个已存在的 `fs.FS` 实例”适配成 `FSProvider` 的通用桥（`文件: mux.go:97`）。它没有 provider 自己的 New，而是在每次 `New(u)` 时根据 `u.Path` 决定 sub 目录（`文件: mux.go:99` 到 `文件: mux.go:107`）：

- `u.Path == "/"`（根 URL，如 `mem:///`）→ `dir = "."` → `fs.Sub(fsys, ".")`。标准库 `fs.Sub` 对 `.` 原样返回 fsys，因此返回的就是底层对象本身（标准库 `io/fs/sub.go`：`dir == "."` 分支）。
- `u.Path` 以 `/` 开头 → `strings.TrimLeft(dir, "/")` 去掉全部前导斜杠后 `fs.Sub`（`文件: mux.go:103` 到 `文件: mux.go:104`）。
- 其余情况（opaque/相对 path）直接 `fs.Sub(fsys, u.Path)`。

`fs.Sub` 的分派规则（标准库）：dir 为 `.` 返回原对象；否则若 fsys 实现了 `fs.SubFS` 调它的 `Sub(dir)`；否则包一层通用 `subFS`，其 `Open(name)` 等价于 `fsys.Open(path.Join(dir, name))`，并透明转发 ReadDir/ReadFile/Glob 等。注意：`fs.Sub` 不检查目录是否存在，也不是安全沙箱（标准库 `fs.Sub` 文档明确说明符号链接可逃逸）。

## 4. 三种 With 扩展为何对不支持能力的实现返回原对象

`WithContextFS`/`WithHeaderFS`/`WithHTTPClientFS` 的实现是同一个模式（`文件: extensions.go:17` 到 `文件: extensions.go:43`）：

```
if cfsys, ok := fsys.(internal.WithContexter); ok { return cfsys.WithContext(ctx) }
return fsys
```

- 能力由“运行时接口断言”决定，而不是编译期参数类型。三个接口都只要求一个返回新 `fs.FS` 的方法（`文件: internal/types.go:85`、`文件: internal/types.go:90`、`文件: internal/types.go:95`）。
- 断言失败时返回**同一个** `fsys`，而不是报错或包一层：因为这些能力是可选的，调用方仍应能用基本 `Open/ReadFile`；包一层反而会隐藏底层可能实现的其他可选接口（如 `fs.ReadDirFS`、`fs.StatFS`、`WithIMDSFSer`）。这是有意的“无能力即透传”。
- 即使断言成功，各 provider 也采用“拷贝结构体 + 改字段 + 返回新指针”的值语义，并且对 `nil` 入参返回接收者本身（如 `文件: httpfs/http.go:59` 到 `文件: httpfs/http.go:97`、`文件: blobfs/blob.go:94` 到 `文件: blobfs/blob.go:124`、`文件: gcpmetafs/gcpmetafs.go:85` 到 `文件: gcpmetafs/gcpmetafs.go:115`），保证链式 With 不共享可变状态。
- 各 provider 在源码里用空白断言显式声明自己支持哪些能力，便于核对：`httpFS` 支持全部三种（`文件: httpfs/http.go:48` 到 `文件: httpfs/http.go:52`）；`blobFS` 支持 context/client 但**不**支持 header（`文件: blobfs/blob.go:82` 到 `文件: blobfs/blob.go:87`）；`fileFS` 三种都不支持（`文件: filefs/file.go:60` 到 `文件: filefs/file.go:66` 只有标准 io/fs 接口）。

## 5. 三个可复现风险

### 风险 A：scheme 重复注册被静默覆盖

- 输入：两个 provider 用相同 scheme 注册，如 `m.Add(FSProviderFunc(p1, "x"))` 后 `m.Add(FSProviderFunc(p2, "x"))`。
- 经过的函数：`FSMux.Add` 对每个 scheme 执行 `m[scheme] = fs.New`（`文件: mux.go:28` 到 `文件: mux.go:31`）；`Lookup → New` 再用 `m[u.Scheme]` 取值（`文件: mux.go:59`）。
- 当前结果：后注册者无条件覆盖先注册者，无错误、无日志；`Schemes()` 基于 map 去重排序（`文件: mux.go:46` 到 `文件: mux.go:54`），同名 scheme 只会出现一次。该行为由测试 `TestUnderstandingProofDispatchOverride` 打印并断言（winner=`second`）。
- 规范定性：**规范允许**。文档注释明确写了“already registered … will be overridden”（`文件: mux.go:26` 到 `文件: mux.go:27`）。
- 业务影响：属于“按设计可踩的坑”而非崩溃缺陷。若不同包意外注册同一 scheme（例如多个 `file`/`http` 变体、或第三方 `git+https`），加载顺序决定真实后端，且 map 迭代/注册顺序不稳定时排查困难。建议需要审计的团队在自己的装配代码里对 `Schemes()` 交集做启动期校验；不应改库本身语义。

### 风险 B：带编码斜杠 / 非根路径的子目录边界

- 输入：`WrappedFSProvider` + URL `mem:///root%2Fsub`（编码斜杠）、`mem:///root/sub`（普通非根路径）、以及 `mem:///root/../etc`（点段）。
- 经过的函数：`url.Parse`（在 `文件: mux.go:37`，`%2F` 在解析期被解码进 `u.Path`）→ `WrappedFSProvider` 的 newFunc 读 `u.Path`（`文件: mux.go:100`）→ `strings.TrimLeft`（`文件: mux.go:104`）→ 标准库 `fs.Sub`（`文件: mux.go:107`）。
- 当前结果（`TestUnderstandingProofSubBoundary` 实测打印）：
  - `%2F` 解码后 `u.Path == "/root/sub"`，与真实斜杠输入产生**完全相同**的 sub 根——层无法区分“名字里带斜杠的单个段”和“两级目录”。
  - 非根路径形成真实 chroot 视图：对返回 FS 调 `Open("../rootfile.txt")` 被 `fs.ValidPath` 拒绝（`invalid argument`），ReadDir(".") 只看到 `subsub`。
  - `/root/../etc` 这种点段 `url.Parse` 原样保留（不做 normalize），最终在 `fs.Sub` 的 `ValidPath` 处返回 `sub root/../etc: invalid argument`。
- 规范定性：边界拒绝 `..` 与非法名是 **io/fs 规范要求**（`fs.ValidPath`），属于正确行为；`%2F` 被解码是 **RFC 3986/`net/url` 的标准行为**，也符合规范。
- 潜在缺陷：层对 `%2F` 无任何区分能力。对于后端把“编码斜杠”当字面量的系统（部分对象存储、网关会区别对待 `a%2Fb` 与 `a/b`），go-fsimpl 在进入 provider 前就把两者归一化，可能造成对象定位歧义或前缀枚举偏差；此外 provider 自持 root 的实现（blob/gcpmeta 用 `path.Join`，`文件: blobfs/blob.go:153`）会进一步折叠多余斜杠。需要保留编码斜杠语义的调用方应绕开 URL path、改用 provider 专有参数。
- 另一个边界细节（规范允许但需知悉）：标准库 `fs.Sub` 不校验目录存在，也不阻止符号链接逃逸（`io/fs/sub.go` 的 `Sub` 文档）；file 链同样不提供 chroot 安全保证。

### 风险 C：包装后可选接口身份丢失

- 输入：一个实现了 `internal.WithContexter` 但**不**实现 `fs.SubFS` 的 FS（测试中的 `proofCapFS`），经 `WrappedFSProvider(...).New("cap:///dir")` 后再调 `WithContextFS`。
- 经过的函数：`WrappedFSProvider` → 标准库 `fs.Sub`（`文件: mux.go:107`）。因底层没有 `Sub` 方法，标准库返回内部适配器类型 `*fs.subFS`（只实现 `Open/ReadDir/ReadFile/ReadLink/Lstat/Glob/Sub`）；随后 `WithContextFS` 对它做 `fsys.(internal.WithContexter)` 断言（`文件: extensions.go:18`）。
- 当前结果（`TestUnderstandingProofCapabilityIdentity` 打印）：sub 前对象是 `*fsimpl.proofCapFS`，`WithContext` 命中（`ctx-hit`），而 `WithHeader`/`WithHTTPClient` 因类型不实现对应接口而返回原对象；sub 后对象变成 `*fs.subFS`，`stillImplementsWithContext=false`，`WithContextFS` 静默返回同一个适配器（`returnedSame=true`），注入的 context/header/client 全部丢失，但普通 ReadFile 仍可用。
- 规范定性：helper 返回原对象是**设计约定**（`文件: extensions.go:14` 到 `文件: extensions.go:43` 注释“if the filesystem supports it”）。
- 潜在缺陷/业务影响：调用顺序敏感——若先做基于 `fs.Sub`/WrappedFSProvider 的 sub 再 With，能力注入会悄无声息失效（取消信号、自定义 `http.Client`、鉴权 header 都不会到达后端），且没有错误提示。排查要点是断言必须发生在最终 FS 上；可靠顺序是**先 With 再 Sub**（对自带 Sub 的 `httpFS`/`blobFS` 有效，因为它们的 `Sub` 返回的仍是具体类型并保留字段，见 `文件: httpfs/http.go:138`、`文件: blobfs/blob.go:143`），而对 `WrappedFSProvider` 这种走通用适配器的路径则无解，属于需要在装配层规避的真实坑。

## 6. 证明测试与复现

文件 `understanding_proof_test.go`（根包 `fsimpl`）含三个测试，全部纯内存：

- `TestUnderstandingProofDispatchOverride`：证明风险 A，打印实际分派 scheme、胜出后端与去重后的 schemes。
- `TestUnderstandingProofSubBoundary`：证明风险 B 的两个不变量（根 URL 返回同一对象；非根路径形成 chroot 边界；`%2F` 与 `/` 归一；`..` 被拒），打印每次 sub 路径与 ReadDir 结果。
- `TestUnderstandingProofCapabilityIdentity`：证明风险 C，打印具体类型、三种能力命中情况和 With 是否返回同一对象。

运行：

```sh
go test -run 'TestUnderstandingProof' -count=1 -v .
```

无网络/云/git daemon/consul 依赖；未修改任何公开 API（仅新增 `_test.go` 与补回缺失的内部包 `internal/env`）。
