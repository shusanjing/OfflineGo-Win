// 2025年12月29日
// OfflineGo - 断网守卫：【断网→关机】功能的桌面应用，专为无通讯功能的 UPS 供电 PC 设计。
// 核心逻辑：当检测到网络断开时，程序会启动一个倒计时，计时结束后执行预设的系统命令（如休眠关机或运行脚本）。
// 特色功能：托盘图标，GUI 交互、日志审计，静默运行、内存优化等。

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall" // 用于在执行外部命令时隐藏控制台窗口
	"time"

	"github.com/go-ping/ping"
	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"gopkg.in/ini.v1"
)

// --- 新增：内存优化相关的底层调用 ---
var (
	libKernel32           = syscall.NewLazyDLL("kernel32.dll")
	procSetProcessWSS     = libKernel32.NewProc("SetProcessWorkingSetSize")
	procGetCurrentProcess = libKernel32.NewProc("GetCurrentProcess")
)

// EmptyWorkingSet 强制修剪进程工作集，释放物理内存占用
func EmptyWorkingSet() {
	handle, _, _ := procGetCurrentProcess.Call()
	// 传入 -1, -1 触发系统回收物理内存
	procSetProcessWSS.Call(handle, ^uintptr(0), ^uintptr(0))
}

// AppConfig 定义了程序的全局配置项
type AppConfig struct {
	TargetIP          string // 监控的目标 IP 地址
	PingInterval      int    // 检测时间间隔（秒）
	PingTimeout       int    // 单次 Ping 的超时时间（秒）
	RetryCount        int    // 触发报警前的最大失败重试次数
	WaitDuration      int    // 断网确认后，执行命令前的缓冲倒计时（秒）
	ActionCommand     string // 倒计时结束执行的系统命令
	AutoHideOnSuccess bool   // 网络恢复后是否自动隐藏主窗口
}

var (
	mw          *walk.MainWindow  // 主窗口实例
	lblMsg      *walk.Label       // 状态消息标签
	ipInput     *walk.LineEdit    // IP 输入框
	ni          *walk.NotifyIcon  // 托盘图标实例（全局化以方便释放）
	isAlerting  bool              // 标记当前是否处于断网报警/倒计时状态
	cancelChan  = make(chan bool) // 用于取消倒计时协程的通道
	configLock  sync.RWMutex      // 保护 cfg 读写的读写锁，确保并发安全
	failCounter int               // 连续 Ping 失败计数器

	// cfg 存储内存中的全局配置，并设置默认值
	cfg = AppConfig{
		TargetIP:          "192.168.123.1",
		PingInterval:      5,
		PingTimeout:       2,
		RetryCount:        3,
		WaitDuration:      180,
		ActionCommand:     "shutdown /h",
		AutoHideOnSuccess: true,
	}
)

func init() {
	// 程序启动时首先加载配置文件
	loadConfig()
}

// writeLog 将日志信息异步写入 log 目录下的日期文件中
func writeLog(message string) {
	logDir := "log"
	// 自动创建日志目录
	if _, err := os.Stat(logDir); os.IsNotExist(err) {
		os.Mkdir(logDir, 0755)
	}

	now := time.Now()
	fileName := fmt.Sprintf("%s/%s.log", logDir, now.Format("2006-01-02"))

	f, err := os.OpenFile(fileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	logLine := fmt.Sprintf("[%s] %s\n", now.Format("15:04:05"), message)
	f.WriteString(logLine)
}

// loadConfig 从本地 config.ini 文件加载配置，若文件不存在则创建默认配置
func loadConfig() {
	cfgFile := "config.ini"
	if _, err := os.Stat(cfgFile); os.IsNotExist(err) {
		saveConfig()
		return
	}

	iniData, err := ini.Load(cfgFile)
	if err != nil {
		return
	}

	// 加载网络相关配置
	sectionM := iniData.Section("Network")
	cfg.TargetIP = sectionM.Key("TargetIP").MustString(cfg.TargetIP)
	cfg.PingInterval = sectionM.Key("PingInterval").MustInt(cfg.PingInterval)
	cfg.PingTimeout = sectionM.Key("PingTimeout").MustInt(cfg.PingTimeout)
	cfg.RetryCount = sectionM.Key("RetryCount").MustInt(cfg.RetryCount)

	// 加载响应动作相关配置
	sectionA := iniData.Section("Actions")
	cfg.WaitDuration = sectionA.Key("WaitDuration").MustInt(cfg.WaitDuration)
	cfg.ActionCommand = sectionA.Key("ActionCommand").MustString(cfg.ActionCommand)
	cfg.AutoHideOnSuccess = sectionA.Key("AutoHideOnSuccess").MustBool(cfg.AutoHideOnSuccess)
}

// saveConfig 将当前内存中的配置保存到 config.ini，并带有详细的注释说明
func saveConfig() {
	f := ini.Empty()

	// 构造 Network 分区
	m, _ := f.NewSection("Network")
	kIP, _ := m.NewKey("TargetIP", cfg.TargetIP)
	kIP.Comment = "监控的目标IP地址："

	kInterval, _ := m.NewKey("PingInterval", fmt.Sprint(cfg.PingInterval))
	kInterval.Comment = "检测间隔（秒）："

	kTimeout, _ := m.NewKey("PingTimeout", fmt.Sprint(cfg.PingTimeout))
	kTimeout.Comment = "单次Ping超时时间（秒）："

	kRetry, _ := m.NewKey("RetryCount", fmt.Sprint(cfg.RetryCount))
	kRetry.Comment = "触发报警前的连续失败次数："

	// 构造 Actions 分区
	a, _ := f.NewSection("Actions")
	kWait, _ := a.NewKey("WaitDuration", fmt.Sprint(cfg.WaitDuration))
	kWait.Comment = "确认断网后，执行命令前的倒计时（秒）："

	kCmd, _ := a.NewKey("ActionCommand", cfg.ActionCommand)
	kCmd.Comment = "倒计时结束时执行的系统命令（若是.bat脚本，切记ANSI编码，调用方式：ActionCommand = D:\\...\\test.bat）："

	kHide, _ := a.NewKey("AutoHideOnSuccess", fmt.Sprint(cfg.AutoHideOnSuccess))
	kHide.Comment = "网络恢复正常后是否自动隐藏窗口 (true/false)："

	f.SaveTo("config.ini")
}

func main() {
	writeLog("--- 程序启动 ---")

	// 构建 GUI 界面
	if err := (MainWindow{
		AssignTo: &mw,
		Title:    "OfflineGo - 断网守卫",
		Icon:     2, // 直接写 ID 即可，walk 会自动调用 NewIconFromResourceId(2)
		MinSize:  Size{Width: 350, Height: 240},
		Layout:   VBox{Margins: Margins{Left: 15, Top: 15, Right: 15, Bottom: 15}, Spacing: 5},
		Visible:  false, // 启动时默认隐藏
		Children: []Widget{
			VSpacer{Size: 1},
			Label{
				AssignTo:  &lblMsg,
				Text:      "正在初始化监控...",
				Font:      Font{PointSize: 10, Bold: true},
				TextColor: walk.RGB(0, 120, 0),
				Alignment: AlignHCenterVCenter,
			},
			VSpacer{Size: 1},
			GroupBox{
				Title:  "监控配置",
				Layout: Grid{Columns: 3},
				Children: []Widget{
					Label{Text: "监控 IP:"},
					LineEdit{
						AssignTo: &ipInput,
						Text:     cfg.TargetIP,
					},
					PushButton{
						Text: "保存配置",
						OnClicked: func() {
							applyAndSave()
						},
					},
				},
			},
			VSpacer{Size: 1},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 5},
				Children: []Widget{
					PushButton{
						Text: "立即执行命令",
						OnClicked: func() {
							if walk.MsgBox(mw, "确认", "确定执行: "+cfg.ActionCommand+" 吗？", walk.MsgBoxIconQuestion|walk.MsgBoxYesNo) == win.IDYES {
								runActionCommand()
							}
						},
					},
					PushButton{
						Text:      "隐藏",
						OnClicked: func() { mw.SetVisible(false); EmptyWorkingSet() },
					},
					PushButton{
						Text:      "退出",
						OnClicked: func() { safeExit() },
					},
				},
			},
			Label{Text: "当前预设命令：" + cfg.ActionCommand},
		},
	}.Create()); err != nil {
		panic(err)
	}

	// 注册窗口关闭事件，确保点击 X 关闭时也能清理托盘
	mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		safeExit()
	})

	setupTray()      // 设置系统托盘图标和菜单
	go monitorLoop() // 在后台协程中启动监控循环

	// 启动后延迟 1 秒执行内存瘦身，确保窗口加载逻辑已完成
	go func() {
		time.Sleep(1 * time.Second)
		EmptyWorkingSet()
	}()

	mw.Run() // 运行 GUI 消息循环
}

// safeExit 统一处理退出逻辑，释放资源
func safeExit() {
	if ni != nil {
		ni.Dispose()
	}
	walk.App().Exit(0)
}

// applyAndSave 处理“保存配置”按钮点击事件，进行即时 Ping 测试并持久化
func applyAndSave() {
	newIP := ipInput.Text()
	writeLog(fmt.Sprintf("手动更新配置，新监控目标: %s", newIP))
	go func() {
		success := doPing(newIP)
		mw.Synchronize(func() {
			configLock.Lock()
			cfg.TargetIP = newIP
			saveConfig()
			configLock.Unlock()
			if success {
				walk.MsgBox(mw, "成功", "测试通过，配置已保存", walk.MsgBoxIconInformation)
			} else {
				walk.MsgBox(mw, "提醒", "已保存，但目前无法 Ping 通该地址", walk.MsgBoxIconWarning)
			}
			EmptyWorkingSet() // 关闭弹窗后触发一次瘦身
		})
	}()
}

// monitorLoop 是核心监控逻辑，定期执行 Ping 并根据结果切换 UI 状态
func monitorLoop() {
	for {
		// 使用读锁获取当前配置
		configLock.RLock()
		currentIP := cfg.TargetIP
		interval := cfg.PingInterval
		maxRetries := cfg.RetryCount
		autoHide := cfg.AutoHideOnSuccess
		configLock.RUnlock()

		success := doPing(currentIP)

		// 通过 Synchronize 在 UI 协程中更新界面
		mw.Synchronize(func() {
			if success {
				if isAlerting {
					// 如果网络恢复，则停止当前的倒计时逻辑
					writeLog(fmt.Sprintf("网络恢复正常: %s (已停止倒计时)", currentIP))
					stopCountdown()

					if autoHide {
						mw.SetVisible(false)
						go EmptyWorkingSet() // 自动隐藏后执行瘦身
					} else {
						lblMsg.SetText(fmt.Sprintf("● 恢复: %s (运行中)", currentIP))
						lblMsg.SetTextColor(walk.RGB(0, 120, 0))
					}
				}
				failCounter = 0

				if !isAlerting {
					lblMsg.SetText(fmt.Sprintf("● 监控中: %s (正常)", currentIP))
					lblMsg.SetTextColor(walk.RGB(0, 120, 0))
				}
			} else {
				failCounter++
				if !isAlerting {
					lblMsg.SetText(fmt.Sprintf("○ 异常: %s (失败 %d/%d)", currentIP, failCounter, maxRetries))
					lblMsg.SetTextColor(walk.RGB(200, 100, 0))

					// 刚好达到重试上限的那一刻记录日志
					if failCounter == maxRetries {
						writeLog(fmt.Sprintf("网络连接断开: %s (连续失败达到 %d 次)", currentIP, maxRetries))
					}
				}
			}
		})

		// 若失败次数达标且未在报警中，启动报警倒计时
		if failCounter >= maxRetries && !isAlerting {
			go startCountdown()
		}

		time.Sleep(time.Duration(interval) * time.Second)
	}
}

// doPing 执行底层 Ping 操作，返回目标是否可达
func doPing(ip string) bool {
	pinger, err := ping.NewPinger(ip)
	if err != nil {
		return false
	}
	pinger.SetPrivileged(true) // 需要管理员权限运行以支持 ICMP
	pinger.Count = 1
	pinger.Timeout = time.Duration(cfg.PingTimeout) * time.Second
	err = pinger.Run()
	return err == nil && pinger.Statistics().PacketsRecv > 0
}

// startCountdown 处理断网后的倒计时逻辑，并在时间到时触发命令
func startCountdown() {
	isAlerting = true
	remaining := cfg.WaitDuration

	mw.Synchronize(func() {
		showMainWindow() // 报警时强制显示窗口
	})

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-cancelChan:
			// 接收到取消信号（通常是因为网络恢复）
			isAlerting = false
			return
		case <-ticker.C:
			remaining--
			mw.Synchronize(func() {
				lblMsg.SetText(fmt.Sprintf("⚠️ 警告：网络中断！\n%d 秒后执行: %s", remaining, cfg.ActionCommand))
				lblMsg.SetTextColor(walk.RGB(255, 0, 0))
			})

			if remaining <= 0 {
				runActionCommand()
				isAlerting = false
				return
			}
		}
	}
}

// stopCountdown 发送信号终止当前的倒计时协程
func stopCountdown() {
	if isAlerting {
		select {
		case cancelChan <- true:
		default:
			// 防止 channel 阻塞
		}
	}
}

// runActionCommand 执行预设的系统命令，并通过 syscall 隐藏 CMD 窗口
func runActionCommand() {
	configLock.RLock()
	fullCmd := cfg.ActionCommand
	configLock.RUnlock()

	if strings.TrimSpace(fullCmd) == "" {
		writeLog("跳过执行：未配置 ActionCommand")
		return
	}

	writeLog(fmt.Sprintf("执行命令: %s", fullCmd))

	// 使用 cmd /C 来运行复合命令或脚本
	cmd := exec.Command("cmd", "/C", fullCmd)

	// 配置 Windows 属性：隐藏 CMD 黑色窗口
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	// 启动命令（非阻塞方式）
	err := cmd.Start()
	if err != nil {
		writeLog(fmt.Sprintf("❌ 启动命令失败: %v", err))
		return
	}

	// 开启异步协程等待命令执行结果，防止主线程阻塞
	go func() {
		err := cmd.Wait()
		if err != nil {
			writeLog(fmt.Sprintf("❌ 命令执行返回错误: %v", err))
		} else {
			writeLog("✅ 命令执行完毕")
		}
	}()
}

// setupTray 初始化托盘图标、气泡提示及托盘右键菜单
func setupTray() {
	// 尝试从资源文件加载图标，若失败则尝试本地文件，最后使用系统默认图标
	icon, _ := walk.NewIconFromResourceId(2)
	if icon == nil {
		icon, _ = walk.NewIconFromFile("app.ico")
	}
	if icon == nil {
		icon, _ = walk.Resources.Icon(fmt.Sprintf("%d", win.IDI_APPLICATION))
	}

	var err error
	ni, err = walk.NewNotifyIcon(mw)
	if err != nil {
		return
	}

	if icon != nil {
		ni.SetIcon(icon)
		// 注意：NotifyIcon 设置 Icon 后，icon 对象本身也应该在不再使用时释放，
		// 但由于此处 icon 全局复用，通常随进程结束。
	}
	ni.SetToolTip("OfflineGo - 断网守卫")
	ni.SetVisible(true)

	// 点击托盘图标时显示窗口
	ni.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			showMainWindow()
		}
	})

	// 构建托盘右键菜单
	showAction := walk.NewAction()
	showAction.SetText("打开主窗口")
	showAction.Triggered().Attach(func() { showMainWindow() })
	ni.ContextMenu().Actions().Add(showAction)

	ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())

	exitAction := walk.NewAction()
	exitAction.SetText("退出程序")
	exitAction.Triggered().Attach(func() {
		writeLog("--- 程序正常退出 ---")
		safeExit()
	})
	ni.ContextMenu().Actions().Add(exitAction)
}

// showMainWindow 处理窗口的显示逻辑，并将其置于屏幕中心最顶层
func showMainWindow() {
	mw.SetVisible(true)
	scrW := win.GetSystemMetrics(win.SM_CXSCREEN)
	scrH := win.GetSystemMetrics(win.SM_CYSCREEN)
	// 设置窗口位置及 TopMost 属性（始终置顶）
	win.SetWindowPos(mw.Handle(), win.HWND_TOPMOST,
		(scrW-350)/3, (scrH-240)/3, 350, 240, win.SWP_SHOWWINDOW)
	win.SetForegroundWindow(mw.Handle())
}
