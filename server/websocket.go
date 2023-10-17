package server

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Mrs4s/MiraiGo/message"
	"github.com/Mrs4s/MiraiGo/utils"
	"github.com/RomiChan/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"

	"github.com/Mrs4s/go-cqhttp/coolq"
	"github.com/Mrs4s/go-cqhttp/global"
	"github.com/Mrs4s/go-cqhttp/modules/api"
	"github.com/Mrs4s/go-cqhttp/modules/config"
	"github.com/Mrs4s/go-cqhttp/modules/filter"
)

var (
	once     sync.Once
	instance *webSocketServer
)

type webSocketServer struct {
	bot  *coolq.CQBot
	conf *WebsocketServer

	mu        sync.Mutex
	eventConn []*wsConn

	token     string
	handshake string
	filter    string
}

// websocketClient WebSocket客户端实例
type websocketClient struct {
	bot       *coolq.CQBot
	mu        sync.Mutex
	universal *wsConn
	event     *wsConn

	token             string
	filter            string
	reconnectInterval time.Duration
	limiter           api.Handler
}

type wsConn struct {
	mu        sync.Mutex
	conn      *websocket.Conn
	apiCaller *api.Caller
	//新增
	currentEcho string
}

func (c *wsConn) WriteText(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second * 15))
	return c.conn.WriteMessage(websocket.TextMessage, b)
}

func (c *wsConn) Close() error {
	return c.conn.Close()
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

const wsDefault = `  # 正向WS设置
  - ws:
      # 正向WS服务器监听地址
      address: 0.0.0.0:8080
      middlewares:
        <<: *default # 引用默认中间件
`

const wsReverseDefault = `  # 反向WS设置
  - ws-reverse:
      # 反向WS Universal 地址
      # 注意 设置了此项地址后下面两项将会被忽略
      universal: ws://your_websocket_universal.server
      # 反向WS API 地址
      api: ws://your_websocket_api.server
      # 反向WS Event 地址
      event: ws://your_websocket_event.server
      # 重连间隔 单位毫秒
      reconnect-interval: 3000
      middlewares:
        <<: *default # 引用默认中间件
`

// WebsocketServer 正向WS相关配置
type WebsocketServer struct {
	Disabled bool   `yaml:"disabled"`
	Address  string `yaml:"address"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`

	MiddleWares `yaml:"middlewares"`
}

// WebsocketReverse 反向WS相关配置
type WebsocketReverse struct {
	Disabled          bool   `yaml:"disabled"`
	Universal         string `yaml:"universal"`
	API               string `yaml:"api"`
	Event             string `yaml:"event"`
	ReconnectInterval int    `yaml:"reconnect-interval"`

	MiddleWares `yaml:"middlewares"`
}

func init() {
	config.AddServer(&config.Server{
		Brief:   "正向 Websocket 通信",
		Default: wsDefault,
	})
	config.AddServer(&config.Server{
		Brief:   "反向 Websocket 通信",
		Default: wsReverseDefault,
	})
}

// // runWSServer 运行一个正向WS server
// func runWSServer(b *coolq.CQBot, node yaml.Node) {
// 	var conf WebsocketServer
// 	switch err := node.Decode(&conf); {
// 	case err != nil:
// 		log.Warn("读取正向Websocket配置失败 :", err)
// 		fallthrough
// 	case conf.Disabled:
// 		return
// 	}

//		network, address := "tcp", conf.Address
//		if conf.Address == "" && (conf.Host != "" || conf.Port != 0) {
//			log.Warn("正向 Websocket 使用了过时的配置格式，请更新配置文件")
//			address = fmt.Sprintf("%s:%d", conf.Host, conf.Port)
//		} else {
//			uri, err := url.Parse(conf.Address)
//			if err == nil && uri.Scheme != "" {
//				network = uri.Scheme
//				address = uri.Host + uri.Path
//			}
//		}
//		s := &webSocketServer{
//			bot:    b,
//			conf:   &conf,
//			token:  conf.AccessToken,
//			filter: conf.Filter,
//		}
//		filter.Add(s.filter)
//		s.handshake = fmt.Sprintf(`{"_post_method":2,"meta_event_type":"lifecycle","post_type":"meta_event","self_id":%d,"sub_type":"connect","time":%d}`,
//			b.Client.Uin, time.Now().Unix())
//		b.OnEventPush(s.onBotPushEvent)
//		mux := http.ServeMux{}
//		mux.HandleFunc("/event", s.event)
//		mux.HandleFunc("/api", s.api)
//		mux.HandleFunc("/", s.any)
//		listener, err := net.Listen(network, address)
//		if err != nil {
//			log.Fatal(err)
//		}
//		log.Infof("CQ WebSocket 服务器已启动: %v", listener.Addr())
//		log.Fatal(http.Serve(listener, &mux))
//	}

func newWebSocketServer(b *coolq.CQBot, conf *WebsocketServer) *webSocketServer {
	s := &webSocketServer{
		bot:       b,
		conf:      conf,
		token:     conf.AccessToken,   // 假设AccessToken在WebsocketServer配置中
		filter:    conf.Filter,        // 假设Filter在WebsocketServer配置中
		eventConn: make([]*wsConn, 0), // 初始化WebSocket连接切片
	}

	s.handshake = fmt.Sprintf(
		`{"_post_method":2,"meta_event_type":"lifecycle","post_type":"meta_event","self_id":%d,"sub_type":"connect","time":%d}`,
		b.Client.Uin,
		time.Now().Unix(),
	)

	return s
}

// InitializeWebSocketServerInstance initializes the singleton instance with the provided configuration.
func InitializeWebSocketServerInstance(b *coolq.CQBot, conf *WebsocketServer) *webSocketServer {
	once.Do(func() {
		instance = newWebSocketServer(b, conf)
	})
	return instance
}

// GetWebSocketServerInstance retrieves the singleton instance, assuming it has been initialized.
func GetWebSocketServerInstance() *webSocketServer {
	if instance == nil {
		log.Fatal("Tried to get the WebSocketServer instance before it was initialized.")
	}
	return instance
}

func runWSServer(b *coolq.CQBot, node yaml.Node) {
	var conf WebsocketServer
	if err := node.Decode(&conf); err != nil {
		log.Warn("读取正向Websocket配置失败 :", err)
		return
	}

	if conf.Disabled {
		return
	}

	network, address := "tcp", conf.Address
	if conf.Address == "" && (conf.Host != "" || conf.Port != 0) {
		log.Warn("正向 Websocket 使用了过时的配置格式，请更新配置文件")
		address = fmt.Sprintf("%s:%d", conf.Host, conf.Port)
	} else {
		uri, err := url.Parse(conf.Address)
		if err == nil && uri.Scheme != "" {
			network = uri.Scheme
			address = uri.Host + uri.Path
		}
	}

	// 初始化全局单例
	s := InitializeWebSocketServerInstance(b, &conf)

	filter.Add(s.filter)
	s.handshake = fmt.Sprintf(`{"_post_method":2,"meta_event_type":"lifecycle","post_type":"meta_event","self_id":%d,"sub_type":"connect","time":%d}`,
		b.Client.Uin, time.Now().Unix())
	b.OnEventPush(s.onBotPushEvent)

	mux := http.ServeMux{}
	mux.HandleFunc("/event", s.event)
	mux.HandleFunc("/api", s.api)
	mux.HandleFunc("/", s.any)

	listener, err := net.Listen(network, address)
	if err != nil {
		log.Fatal(err)
	}
	log.Infof("CQ WebSocket 服务器已启动: %v", listener.Addr())
	log.Fatal(http.Serve(listener, &mux))
}

// runWSClient 运行一个反向向WS client
func runWSClient(b *coolq.CQBot, node yaml.Node) {
	var conf WebsocketReverse
	switch err := node.Decode(&conf); {
	case err != nil:
		log.Warn("读取反向Websocket配置失败 :", err)
		fallthrough
	case conf.Disabled:
		return
	}

	c := &websocketClient{
		bot:    b,
		token:  conf.AccessToken,
		filter: conf.Filter,
	}
	filter.Add(c.filter)

	if conf.ReconnectInterval != 0 {
		c.reconnectInterval = time.Duration(conf.ReconnectInterval) * time.Millisecond
	} else {
		c.reconnectInterval = time.Second * 5
	}

	if conf.RateLimit.Enabled {
		c.limiter = rateLimit(conf.RateLimit.Frequency, conf.RateLimit.Bucket)
	}

	if conf.Universal != "" {
		c.connect("Universal", conf.Universal, &c.universal)
		c.bot.OnEventPush(c.onBotPushEvent("Universal", conf.Universal, &c.universal))
		return // 连接到 Universal 后， 不再连接其他
	}
	if conf.API != "" {
		c.connect("API", conf.API, nil)
	}
	if conf.Event != "" {
		c.connect("Event", conf.Event, &c.event)
		c.bot.OnEventPush(c.onBotPushEvent("Event", conf.Event, &c.event))
	}
}

func resolveURI(addr string) (network, address string) {
	network, address = "tcp", addr
	uri, err := url.Parse(addr)
	if err == nil && uri.Scheme != "" {
		scheme, ext, _ := strings.Cut(uri.Scheme, "+")
		if ext != "" {
			network = ext
			uri.Scheme = scheme // remove `+unix`/`+tcp4`
			if ext == "unix" {
				uri.Host, uri.Path, _ = strings.Cut(uri.Path, ":")
				uri.Host = base64.StdEncoding.EncodeToString([]byte(uri.Host))
			}
			address = uri.String()
		}
	}
	return
}

func (c *websocketClient) connect(typ, addr string, conptr **wsConn) {
	log.Infof("开始尝试连接到反向WebSocket %s服务器: %v", typ, addr)
	header := http.Header{
		"X-Client-Role": []string{typ},
		"X-Self-ID":     []string{strconv.FormatInt(c.bot.Client.Uin, 10)},
		"User-Agent":    []string{"CQHttp/4.15.0"},
	}
	log.Infof("请求头:%s", header)
	if c.token != "" {
		header["Authorization"] = []string{"Token " + c.token}
	}

	network, address := resolveURI(addr)
	dialer := websocket.Dialer{
		NetDial: func(_, addr string) (net.Conn, error) {
			if network == "unix" {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					host = addr
				}
				filepath, err := base64.RawURLEncoding.DecodeString(host)
				if err == nil {
					addr = string(filepath)
				}
			}
			return net.Dial(network, addr) // support unix socket transport
		},
	}

	conn, _, err := dialer.Dial(address, header) // nolint
	if err != nil {
		log.Warnf("连接到反向WebSocket %s服务器 %v 时出现错误: %v", typ, addr, err)
		if c.reconnectInterval != 0 {
			time.Sleep(c.reconnectInterval)
			c.connect(typ, addr, conptr)
		}
		return
	}

	switch typ {
	case "Event", "Universal":
		handshake := fmt.Sprintf(`{"meta_event_type":"lifecycle","post_type":"meta_event","self_id":%d,"sub_type":"connect","time":%d}`, c.bot.Client.Uin, time.Now().Unix())
		err = conn.WriteMessage(websocket.TextMessage, []byte(handshake))
		if err != nil {
			log.Warnf("反向WebSocket 握手时出现错误: %v", err)
		}
	}

	log.Infof("已连接到反向WebSocket %s服务器 %v", typ, addr)

	var wrappedConn *wsConn
	if conptr != nil && *conptr != nil {
		wrappedConn = *conptr
	} else {
		wrappedConn = new(wsConn)
		if conptr != nil {
			*conptr = wrappedConn
		}
	}

	wrappedConn.conn = conn
	wrappedConn.apiCaller = api.NewCaller(c.bot)
	if c.limiter != nil {
		wrappedConn.apiCaller.Use(c.limiter)
	}

	if typ != "Event" {
		go c.listenAPI(typ, addr, wrappedConn)
	}
}

func (c *websocketClient) listenAPI(typ, url string, conn *wsConn) {
	defer func() { _ = conn.Close() }()
	for {
		buffer := global.NewBuffer()
		t, reader, err := conn.conn.NextReader()
		if err != nil {
			log.Warnf("监听反向WS %s时出现错误: %v", typ, err)
			break
		}
		_, err = buffer.ReadFrom(reader)
		if err != nil {
			log.Warnf("监听反向WS %s时出现错误: %v", typ, err)
			break
		}
		if t == websocket.TextMessage {
			go func(buffer *bytes.Buffer) {
				defer global.PutBuffer(buffer)
				conn.handleRequest(c.bot, buffer.Bytes())
			}(buffer)
		} else {
			global.PutBuffer(buffer)
		}
	}
	if c.reconnectInterval != 0 {
		time.Sleep(c.reconnectInterval)
		if typ == "API" { // Universal 不重连，避免多次重连
			go c.connect(typ, url, nil)
		}
	}
}

func (c *websocketClient) onBotPushEvent(typ, url string, conn **wsConn) func(e *coolq.Event) {
	return func(e *coolq.Event) {
		c.mu.Lock()
		defer c.mu.Unlock()

		flt := filter.Find(c.filter)
		if flt != nil && !flt.Eval(gjson.Parse(e.JSONString())) {
			log.Debugf("上报Event %s 到 WS服务器 时被过滤.", e.JSONBytes())
			return
		}

		log.Debugf("向反向WS %s服务器推送Event: %s", typ, e.JSONBytes())
		if err := (*conn).WriteText(e.JSONBytes()); err != nil {
			log.Warnf("向反向WS %s服务器推送 Event 时出现错误: %v", typ, err)
			_ = (*conn).Close()
			if c.reconnectInterval != 0 {
				time.Sleep(c.reconnectInterval)
				c.connect(typ, url, conn)
			}
		}
	}
}

func (s *webSocketServer) event(w http.ResponseWriter, r *http.Request) {
	status := checkAuth(r, s.token)
	if status != http.StatusOK {
		log.Warnf("已拒绝 %v 的 WebSocket 请求: Token鉴权失败(code:%d)", r.RemoteAddr, status)
		w.WriteHeader(status)
		return
	}

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Warnf("处理 WebSocket 请求时出现错误: %v", err)
		return
	}

	err = c.WriteMessage(websocket.TextMessage, []byte(s.handshake))
	if err != nil {
		log.Warnf("WebSocket 握手时出现错误: %v", err)
		_ = c.Close()
		return
	}

	log.Infof("接受 WebSocket 连接: %v (/event)", r.RemoteAddr)
	conn := &wsConn{conn: c, apiCaller: api.NewCaller(s.bot)}
	s.mu.Lock()
	s.eventConn = append(s.eventConn, conn)
	s.mu.Unlock()
}

func (s *webSocketServer) api(w http.ResponseWriter, r *http.Request) {
	status := checkAuth(r, s.token)
	if status != http.StatusOK {
		log.Warnf("已拒绝 %v 的 WebSocket 请求: Token鉴权失败(code:%d)", r.RemoteAddr, status)
		w.WriteHeader(status)
		return
	}

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Warnf("处理 WebSocket 请求时出现错误: %v", err)
		return
	}

	log.Infof("接受 WebSocket 连接: %v (/api)", r.RemoteAddr)
	conn := &wsConn{conn: c, apiCaller: api.NewCaller(s.bot)}
	if s.conf.RateLimit.Enabled {
		conn.apiCaller.Use(rateLimit(s.conf.RateLimit.Frequency, s.conf.RateLimit.Bucket))
	}
	s.listenAPI(conn)
}

func (s *webSocketServer) any(w http.ResponseWriter, r *http.Request) {
	status := checkAuth(r, s.token)
	if status != http.StatusOK {
		log.Warnf("已拒绝 %v 的 WebSocket 请求: Token鉴权失败(code:%d)", r.RemoteAddr, status)
		w.WriteHeader(status)
		return
	}

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Warnf("处理 WebSocket 请求时出现错误: %v", err)
		return
	}

	err = c.WriteMessage(websocket.TextMessage, []byte(s.handshake))
	if err != nil {
		log.Warnf("WebSocket 握手时出现错误: %v", err)
		_ = c.Close()
		return
	}

	log.Infof("接受 WebSocket 连接: %v (/)", r.RemoteAddr)
	conn := &wsConn{conn: c, apiCaller: api.NewCaller(s.bot)}
	if s.conf.RateLimit.Enabled {
		conn.apiCaller.Use(rateLimit(s.conf.RateLimit.Frequency, s.conf.RateLimit.Bucket))
	}
	s.mu.Lock()
	s.eventConn = append(s.eventConn, conn)
	s.mu.Unlock()
	s.listenAPI(conn)
}

type DynamicInt64 int64

type WebSocketMessage struct {
	PostType    string       `json:"post_type"`
	MessageType string       `json:"message_type"`
	Time        DynamicInt64 `json:"time"`
	SelfID      DynamicInt64 `json:"self_id"`
	SubType     string       `json:"sub_type"`
	Sender      struct {
		Age      int          `json:"age"`
		Card     string       `json:"card"`
		Nickname string       `json:"nickname"`
		Role     string       `json:"role"`
		UserID   DynamicInt64 `json:"user_id"`
	} `json:"sender"`
	UserID         DynamicInt64 `json:"user_id"`
	MessageID      DynamicInt64 `json:"message_id"`
	GroupID        DynamicInt64 `json:"group_id"`
	MessageContent interface{}  `json:"message"`
	MessageSeq     DynamicInt64 `json:"message_seq"`
	RawMessage     string       `json:"raw_message"`
	Echo           string       `json:"echo,omitempty"`
}

// 新增
type BasicMessage struct {
	Echo string          `json:"echo"`
	Data json.RawMessage `json:"data"`
}

var md5Int64Mapping = make(map[int64]string)
var md5Int64MappingLock sync.Mutex

func (d *DynamicInt64) UnmarshalJSON(b []byte) error {
	var n int64
	var s string

	// 首先尝试解析为字符串
	if err := json.Unmarshal(b, &s); err == nil {
		// 如果解析成功，再尝试将字符串解析为int64
		n, err = strconv.ParseInt(s, 10, 64)
		if err != nil { // 如果是非数字字符串
			// 进行 md5 处理
			hashed := md5.Sum([]byte(s))
			hexString := hex.EncodeToString(hashed[:])

			// 去除字母并取前15位
			numericPart := strings.Map(func(r rune) rune {
				if unicode.IsDigit(r) {
					return r
				}
				return -1
			}, hexString)

			if len(numericPart) < 15 {
				return fmt.Errorf("哈希过短: %s", numericPart)
			}

			n, err = strconv.ParseInt(numericPart[:15], 10, 64)
			if err != nil {
				return err
			}

			// 更新映射，但限制锁的范围
			md5Int64MappingLock.Lock()
			md5Int64Mapping[n] = s
			md5Int64MappingLock.Unlock()
		}
	} else {
		// 如果字符串解析失败，再尝试解析为int64
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
	}

	*d = DynamicInt64(n)
	return nil
}

func (d DynamicInt64) ToInt64() int64 {
	return int64(d)
}

func (d DynamicInt64) ToString() string {
	return strconv.FormatInt(int64(d), 10)
}

// func parseEcho(echo string) (string, string) {
// 	parts := strings.SplitN(echo, ":", 2)
// 	if len(parts) == 2 {
// 		return parts[0], parts[1]
// 	}
// 	return echo, ""
// }

// 假设 wsmsg.Message 是一个字符串，包含了形如 [CQ:at=xxxx] 的数据
func extractAtElements(str string) []*message.AtElement {
	re := regexp.MustCompile(`\[CQ:at=(\d+)\]`) // 正则表达式匹配[CQ:at=xxxx]其中xxxx是数字
	matches := re.FindAllStringSubmatch(str, -1)

	var elements []*message.AtElement
	for _, match := range matches {
		if len(match) == 2 {
			target, err := strconv.ParseInt(match[1], 10, 64) // 转化xxxx为int64
			if err != nil {
				continue // 如果转化失败，跳过此次循环
			}
			elements = append(elements, &message.AtElement{
				Target: target,
				// 如果有 Display 和 SubType 的信息，也可以在这里赋值
			})
		}
	}
	return elements
}

// 正向ws收信息的地方
func (s *webSocketServer) listenAPI(c *wsConn) {
	// 在这里是gocq作为服务端收到信息的地方, shamrock上报事件
	defer func() { _ = c.Close() }()
	for {
		buffer := global.NewBuffer()
		t, reader, err := c.conn.NextReader()
		if err != nil {
			log.Println(err)
			break
		}
		_, err = buffer.ReadFrom(reader)
		if err != nil {
			log.Println(err)
			break
		}

		if t == websocket.TextMessage {
			go func(buffer *bytes.Buffer) {
				defer global.PutBuffer(buffer)
				data := buffer.Bytes()

				// 打印收到的消息
				log.Println("gocq正向ws服务器收到信息:", string(data))

				// 初步解析为 BasicMessage
				var basicMsg BasicMessage
				err = json.Unmarshal(data, &basicMsg)
				if err != nil {
					log.Println("Failed to parse basic message:", err)
					return
				}

				// 解析消息为 WebSocketMessage
				var wsmsg WebSocketMessage
				err = json.Unmarshal(data, &wsmsg)
				if err != nil {
					log.Println("Failed to parse message:", err)
					return
				}

				// 存储 echo
				c.currentEcho = wsmsg.Echo

				// 处理解析后的消息
				if wsmsg.MessageType == "group" {
					g := &message.GroupMessage{
						Id:        int32(wsmsg.MessageSeq),
						GroupCode: wsmsg.GroupID.ToInt64(),
						GroupName: wsmsg.Sender.Card,
						Sender: &message.Sender{
							Uin:      wsmsg.Sender.UserID.ToInt64(),
							Nickname: wsmsg.Sender.Nickname,
							CardName: wsmsg.Sender.Card,
							IsFriend: false,
						},
						Time:           int32(wsmsg.Time),
						OriginalObject: nil,
					}
					if MessageContent, ok := wsmsg.MessageContent.(string); ok {
						// 替换字符串中的"\/"为"/"
						MessageContent = strings.Replace(MessageContent, "\\/", "/", -1)
						// 使用extractAtElements函数从wsmsg.Message中提取At元素
						atElements := extractAtElements(MessageContent)
						// 将提取的At元素和文本元素都添加到g.Elements
						g.Elements = append(g.Elements, &message.TextElement{Content: MessageContent})
						for _, elem := range atElements {
							g.Elements = append(g.Elements, elem)
						}
					} else if contentArray, ok := wsmsg.MessageContent.([]interface{}); ok {
						for _, contentInterface := range contentArray {
							contentMap, ok := contentInterface.(map[string]interface{})
							if !ok {
								continue
							}

							contentType, ok := contentMap["type"].(string)
							if !ok {
								continue
							}

							switch contentType {
							case "text":
								text, ok := contentMap["data"].(map[string]interface{})["text"].(string)
								if ok {
									// 替换字符串中的"\/"为"/"
									text = strings.Replace(text, "\\/", "/", -1)
									g.Elements = append(g.Elements, &message.TextElement{Content: text})
								}
							case "at":
								if data, ok := contentMap["data"].(map[string]interface{}); ok {
									if qqData, ok := data["qq"].(float64); ok {
										qq := strconv.Itoa(int(qqData))
										atText := fmt.Sprintf("[CQ:at,qq=%s]", qq)
										g.Elements = append(g.Elements, &message.TextElement{Content: atText})
									}
								}
							}
						}
					}
					fmt.Println("准备c.GroupMessageEvent.dispatch(c, g)")
					fmt.Printf("%+v\n", g)
					// 使用 dispatch 方法
					s.bot.Client.GroupMessageEvent.Dispatch(s.bot.Client, g)
				}
				if wsmsg.MessageType == "private" {
					pMsg := &message.PrivateMessage{
						Id:         int32(wsmsg.MessageID),
						InternalId: 0,
						Self:       wsmsg.SelfID.ToInt64(),
						Target:     wsmsg.Sender.UserID.ToInt64(),
						Time:       int32(wsmsg.Time),
						Sender: &message.Sender{
							Uin:      wsmsg.Sender.UserID.ToInt64(),
							Nickname: wsmsg.Sender.Nickname,
							CardName: "", // Private message might not have a Card
							IsFriend: true,
						},
					}

					if MessageContent, ok := wsmsg.MessageContent.(string); ok {
						// 替换字符串中的"\/"为"/"
						MessageContent = strings.Replace(MessageContent, "\\/", "/", -1)
						// 使用extractAtElements函数从wsmsg.Message中提取At元素
						atElements := extractAtElements(MessageContent)
						// 将提取的At元素和文本元素都添加到g.Elements
						pMsg.Elements = append(pMsg.Elements, &message.TextElement{Content: MessageContent})
						for _, elem := range atElements {
							pMsg.Elements = append(pMsg.Elements, elem)
						}
					} else if contentArray, ok := wsmsg.MessageContent.([]interface{}); ok {
						for _, contentInterface := range contentArray {
							contentMap, ok := contentInterface.(map[string]interface{})
							if !ok {
								continue
							}

							contentType, ok := contentMap["type"].(string)
							if !ok {
								continue
							}

							switch contentType {
							case "text":
								text, ok := contentMap["data"].(map[string]interface{})["text"].(string)
								if ok {
									// 替换字符串中的"\/"为"/"
									text = strings.Replace(text, "\\/", "/", -1)
									pMsg.Elements = append(pMsg.Elements, &message.TextElement{Content: text})
								}
							case "at":
								if data, ok := contentMap["data"].(map[string]interface{}); ok {
									if qqData, ok := data["qq"].(float64); ok {
										qq := strconv.Itoa(int(qqData))
										atText := fmt.Sprintf("[CQ:at,qq=%s]", qq)
										pMsg.Elements = append(pMsg.Elements, &message.TextElement{Content: atText})
									}
								}
							}
						}
					}
					selfIDStr := strconv.FormatInt(int64(wsmsg.SelfID), 10)
					if selfIDStr == strconv.FormatInt(int64(wsmsg.Sender.UserID), 10) {
						s.bot.Client.SelfPrivateMessageEvent.Dispatch(s.bot.Client, pMsg)
					} else {
						s.bot.Client.PrivateMessageEvent.Dispatch(s.bot.Client, pMsg)
					}
				}
			}(buffer)
		} else {
			global.PutBuffer(buffer)
		}
	}
}

func (c *wsConn) handleRequest(_ *coolq.CQBot, payload []byte) {
	defer func() {
		if err := recover(); err != nil {
			log.Errorf("处置WS命令时发生无法恢复的异常：%v\n%s", err, debug.Stack())
			_ = c.Close()
		}
	}()

	j := gjson.Parse(utils.B2S(payload))
	t := strings.TrimSuffix(j.Get("action").Str, "_async")
	params := j.Get("params")
	log.Warnf("gocq反向WS接收到API调用: %v 参数: %v", t, params.Raw)
	// ret := c.apiCaller.Call(t, onebot.V11, params)
	// if j.Get("echo").Exists() {
	// 	ret["echo"] = j.Get("echo").Value()
	// }

	// c.mu.Lock()
	// defer c.mu.Unlock()
	// _ = c.conn.SetWriteDeadline(time.Now().Add(time.Second * 15))
	// writer, err := c.conn.NextWriter(websocket.TextMessage)
	// if err != nil {
	// 	log.Errorf("无法响应API调用(连接已断开?): %v", err)
	// 	return
	// }
	// _ = json.NewEncoder(writer).Encode(ret)
	// _ = writer.Close()

	// 直接将原始payload传递给onBotPushEvent_函数处理
	// 获取webSocketServer的单例实例
	server := GetWebSocketServerInstance()

	// 调用单例实例中的方法
	server.onBotPushEvent_(payload)
}

func (s *webSocketServer) onBotPushEvent_(payload []byte) {
	// 删除所有与 coolq.Event 相关的代码，因为我们不再处理它

	s.mu.Lock()
	defer s.mu.Unlock()

	j := 0
	for i := 0; i < len(s.eventConn); i++ {
		conn := s.eventConn[i]
		log.Errorf("gocq正向ws服务端向WS客户端转发action: %s", payload)
		if err := conn.WriteText(payload); err != nil { // 直接发送原始 payload
			_ = conn.Close()
			conn = nil
			continue
		}
		if i != j {
			// i != j意味着某些连接已经关闭。
			// 使用就地删除以避免复制。
			s.eventConn[j] = conn
		}
		j++
	}
	s.eventConn = s.eventConn[:j]
}

func (s *webSocketServer) onBotPushEvent(e *coolq.Event) {
	// flt := filter.Find(s.filter)
	// if flt != nil && !flt.Eval(gjson.Parse(e.JSONString())) {
	// 	log.Debugf("上报Event %s 到 WS客户端 时被过滤.", e.JSONBytes())
	// 	return
	// }

	// s.mu.Lock()
	// defer s.mu.Unlock()

	// j := 0
	// for i := 0; i < len(s.eventConn); i++ {
	// 	conn := s.eventConn[i]
	// 	log.Debugf("向WS客户端推送Event: %s", e.JSONBytes())
	// 	if err := conn.WriteText(e.JSONBytes()); err != nil {
	// 		_ = conn.Close()
	// 		conn = nil
	// 		continue
	// 	}
	// 	if i != j {
	// 		// i != j means that some connection has been closed.
	// 		// use an in-place removal to avoid copying.
	// 		s.eventConn[j] = conn
	// 	}
	// 	j++
	// }
	// s.eventConn = s.eventConn[:j]
}
