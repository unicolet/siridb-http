package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/astaxie/beego/session"
	"github.com/googollee/go-socket.io"
	siridb "github.com/transceptor-technology/go-siridb-connector"

	"time"

	kingpin "gopkg.in/alecthomas/kingpin.v2"
	ini "gopkg.in/ini.v1"
)

// AppVersion exposes version information
const AppVersion = "2.0.0"

const retryConnectTime = 5

// Conn is used to store the user/password with the client.
type Conn struct {
	user     string
	password string
	client   *siridb.Client
}

type store struct {
	connections   []Conn
	dbname        string
	timePrecision string
	version       string
	servers       []server
	port          uint16
	insertTimeout uint16
	logCh         chan string
	reqAuth       bool
	multiUser     bool
	enableWeb     bool
	enableSio     bool
	enableSSL     bool
	ssessions     map[string]string
	cookieMaxAge  uint64
	crtFile       string
	keyFile       string
	gsessions     *session.Manager
}

type server struct {
	host string
	port uint16
}

var (
	xApp     = kingpin.New("siridb-http", "Provides a HTTP API and optional web interface for SiriDB.")
	xConfig  = xApp.Flag("config", "Configuration and connection file for SiriDB HTTP.").Default("").Short('c').String()
	xVerbose = xApp.Flag("verbose", "Enable verbose logging.").Bool()
	xVersion = xApp.Flag("version", "Print version information and exit.").Bool()
)

var base = store{}

func getHostAndPort(addr string) (server, error) {
	parts := strings.Split(addr, ":")
	// IPv4
	if len(parts) == 1 {
		return server{parts[0], 9000}, nil
	}
	if len(parts) == 2 {
		u, err := strconv.ParseUint(parts[1], 10, 16)
		return server{parts[0], uint16(u)}, err
	}
	// IPv6
	if addr[0] != '[' {
		return server{fmt.Sprintf("[%s]", addr), 9000}, nil
	}
	if addr[len(addr)-1] == ']' {
		return server{addr, 9000}, nil
	}
	u, err := strconv.ParseUint(parts[len(parts)-1], 10, 16)
	addr = strings.Join(parts[:len(parts)-1], ":")

	return server{addr, uint16(u)}, err
}

func getServers(addrstr string) ([]server, error) {
	arr := strings.Split(addrstr, ",")
	servers := make([]server, len(arr))
	for i, addr := range arr {
		addr = strings.TrimSpace(addr)
		server, err := getHostAndPort(addr)
		if err != nil {
			return nil, err
		}
		servers[i] = server
	}
	return servers, nil
}

// ServersToInterface takes a server object and retruns a interface{}
func ServersToInterface(servers []server) [][]interface{} {
	ret := make([][]interface{}, len(servers))
	for i, svr := range servers {
		ret[i] = make([]interface{}, 2)
		ret[i][0] = svr.host
		ret[i][1] = int(svr.port)
	}
	return ret
}

func logHandle(logCh chan string) {
	for {
		msg := <-logCh
		if *xVerbose {
			println(msg)
		}
	}
}

func sigHandle(sigCh chan os.Signal) {
	for {
		<-sigCh
		println("CTRL+C pressed...")
		quit(nil)
	}
}

func quit(err error) {
	rc := 0
	if err != nil {
		fmt.Printf("%s\n", err)
		rc = 1
	}

	for _, conn := range base.connections {
		if conn.client != nil {
			conn.client.Close()
		}
	}

	os.Exit(rc)
}

func connect(conn Conn) {
	for !conn.client.IsConnected() {
		base.logCh <- fmt.Sprintf("not connected to SiriDB, try again in %d seconds", retryConnectTime)
		time.Sleep(retryConnectTime * time.Second)
	}
	res, err := conn.client.Query("show time_precision, version", 10)
	if err != nil {
		quit(err)
	}
	v, ok := res.(map[string]interface{})
	if !ok {
		quit(fmt.Errorf("missing 'map' in data"))
	}

	arr, ok := v["data"].([]interface{})
	if !ok || len(arr) != 2 {
		quit(fmt.Errorf("missing array 'data' or length 2 in map"))
	}

	base.timePrecision, ok = arr[0].(map[string]interface{})["value"].(string)
	base.version, ok = arr[1].(map[string]interface{})["value"].(string)

	if !ok {
		quit(fmt.Errorf("cannot find time_precision and/or version in data"))
	}
}

func readBool(section *ini.Section, key string) (b bool) {
	if bIni, err := section.GetKey(key); err != nil {
		quit(err)
	} else if b, err = bIni.Bool(); err != nil {
		quit(err)
	}
	return b
}

func readString(section *ini.Section, key string) (s string) {
	if sIni, err := section.GetKey(key); err != nil {
		quit(err)
	} else {
		s = sIni.String()
	}
	return s
}

func main() {

	// parse arguments
	_, err := xApp.Parse(os.Args[1:])
	if err != nil {
		quit(err)
	}

	if *xVersion {
		fmt.Printf("Version: %s\n", AppVersion)
		os.Exit(0)
	}

	if *xConfig == "" {
		fmt.Printf(
			`# SiriDB HTTP Configuration file
[Database]
user = iris
password = siri
dbname = dbtest
# Multiple servers are possible and should be comma separated. When a port
# is not provided the default 9000 is used. IPv6 address are supported and
# should be wrapped in square brackets [] in case an alternative port is
# required.
#
# Valid examples:
#   siridb01.local,siridb02.local,siridb03.local,siridb04.local
#   10.20.30.40
#   [::1]:5050,[::1]:5051
#   2001:0db8:85a3:0000:0000:8a2e:0370:7334
servers = localhost:9000

[Configuration]
port = 8080
require_authentication = True
enable_socket_io = True
enable_ssl = False
enable_web = True
# When multi user is disabled, only the user/password combination provided in
# this configuration file can be used to create a session connection to SiriDB.
enable_multi_user = False
cookie_max_age = 604800
insert_timeout = 60
# In case a secret is set, the secret can be used to authenticate each request.
# secret = my_super_secret

[SSL]
# Self-signed certificates can be created using:
#
#   openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
#        -keyout certificate.key -out certificate.crt
#
crt_file = my_certificate.crt
key_file = my_certificate.key

#
# Welcome and thank you for using SiriDB!
#
# A configuration file is required and shoud be provided with the --config <file> argument.
# Above you find an example template which can used.
#

`)
		os.Exit(0)
	}

	var conn Conn

	cfg, err := ini.Load(*xConfig)
	if err != nil {
		quit(err)
	}

	section, err := cfg.GetSection("Database")
	if err != nil {
		quit(err)
	}

	base.servers, err = getServers(readString(section, "servers"))
	if err != nil {
		quit(err)
	}

	base.dbname = readString(section, "dbname")
	conn.user = readString(section, "user")
	conn.password = readString(section, "password")

	base.logCh = make(chan string)
	go logHandle(base.logCh)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go sigHandle(sigCh)

	conn.client = siridb.NewClient(
		conn.user,                        // user
		conn.password,                    // password
		base.dbname,                      // database
		ServersToInterface(base.servers), // siridb server(s)
		base.logCh,                       // optional log channel
	)
	base.connections = append(base.connections, conn)
	base.ssessions = make(map[string]string)

	section, err = cfg.GetSection("Configuration")
	if err != nil {
		quit(err)
	}

	base.reqAuth = readBool(section, "require_authentication")
	base.enableWeb = readBool(section, "enable_web")
	base.enableSio = readBool(section, "enable_socket_io")
	base.enableSSL = readBool(section, "enable_ssl")
	base.multiUser = readBool(section, "enable_multi_user")

	if portIni, err := section.GetKey("port"); err != nil {
		quit(err)
	} else if port64, err := portIni.Uint64(); err != nil {
		quit(err)
	} else {
		base.port = uint16(port64)
	}

	if cookieMaxAgeIni, err := section.GetKey("cookie_max_age"); err != nil {
		quit(err)
	} else if base.cookieMaxAge, err = cookieMaxAgeIni.Uint64(); err != nil {
		quit(err)
	}

	if insertTimeoutIni, err := section.GetKey("insert_timeout"); err != nil {
		quit(err)
	} else if insertTimeout64, err := insertTimeoutIni.Uint64(); err != nil {
		quit(err)
	} else {
		base.insertTimeout = uint16(insertTimeout64)
	}

	if base.enableSSL {
		section, err = cfg.GetSection("SSL")
		if err != nil {
			quit(err)
		}
		base.crtFile = readString(section, "crt_file")
		base.keyFile = readString(section, "key_file")
	}

	http.HandleFunc("*", handlerNotFound)

	if base.enableWeb {
		http.HandleFunc("/", handlerMain)
		http.HandleFunc("/js/bundle", handlerJsBundle)
		http.HandleFunc("/js/jsleri", handlerLeriMinJS)
		http.HandleFunc("/js/grammar", handlerGrammarJS)
		http.HandleFunc("/css/bootstrap", handlerBootstrapCSS)
		http.HandleFunc("/css/layout", handlerLayout)
		http.HandleFunc("/favicon.ico", handlerFaviconIco)
		http.HandleFunc("/img/siridb-large.png", handlerSiriDBLargePNG)
		http.HandleFunc("/img/siridb-small.png", handlerSiriDBSmallPNG)
		http.HandleFunc("/img/loader.gif", handlerLoaderGIF)
		http.HandleFunc("/css/font-awesome.min.css", handlerFontAwesomeMinCSS)
		http.HandleFunc("/fonts/FontAwesome.otf", handlerFontsFaOTF)
		http.HandleFunc("/fonts/fontawesome-webfont.eot", handlerFontsFaEOT)
		http.HandleFunc("/fonts/fontawesome-webfont.svg", handlerFontsFaSVG)
		http.HandleFunc("/fonts/fontawesome-webfont.ttf", handlerFontsFaTTF)
		http.HandleFunc("/fonts/fontawesome-webfont.woff", handlerFontsFaWOFF)
		http.HandleFunc("/fonts/fontawesome-webfont.woff2", handlerFontsFaWOFF2)
	}

	http.HandleFunc("/db-info", handlerDbInfo)
	http.HandleFunc("/auth/fetch", handlerAuthFetch)
	http.HandleFunc("/query", handlerQuery)
	http.HandleFunc("/insert", handlerInsert)

	if base.reqAuth {
		cf := new(session.ManagerConfig)
		cf.EnableSetCookie = true
		s := fmt.Sprintf(`{"cookieName":"siridbadminsessionid","gclifetime":%d}`, base.cookieMaxAge)

		if err = json.Unmarshal([]byte(s), cf); err != nil {
			quit(err)
		}

		if base.gsessions, err = session.NewManager("memory", cf); err != nil {
			quit(err)
		}

		go base.gsessions.GC()
		http.HandleFunc("/auth/login", handlerAuthLogin)
		http.HandleFunc("/auth/logout", handlerAuthLogout)
	}

	conn.client.Connect()
	go connect(conn)

	if base.enableSio {
		server, err := socketio.NewServer(nil)
		if err != nil {
			quit(err)
		}

		server.On("connection", func(so socketio.Socket) {
			so.On("db-info", func(req string) (int, string) {
				return onDbInfo(&so)
			})
			so.On("auth fetch", func(req string) (int, string) {
				return onAuthFetch(&so)
			})
			so.On("auth login", func(req string) (int, string) {
				return onAuthLogin(&so, req)
			})
			so.On("auth logout", func(req string) (int, string) {
				return onAuthLogout(&so)
			})
			so.On("query", func(req string) (int, string) {
				return onQuery(&so, req)
			})
			so.On("insert", func(req string) (int, string) {
				return onInsert(&so, req)
			})
			so.On("disconnection", func() {
				delete(base.ssessions, so.Id())
			})
		})

		server.On("error", func(so socketio.Socket, err error) {
			log.Println("error:", err)
		})

		http.Handle("/socket.io/", server)
	}

	msg := "Serving SiriDB API on http%s://0.0.0.0:%d\nPress CTRL+C to quit\n"
	if base.enableSSL {
		fmt.Printf(msg, "s", base.port)
		if err = http.ListenAndServeTLS(
			fmt.Sprintf(":%d", base.port),
			base.crtFile,
			base.keyFile,
			nil); err != nil {
			fmt.Printf("error: %s\n", err)
		}
	} else {
		fmt.Printf(msg, "", base.port)
		if err = http.ListenAndServe(fmt.Sprintf(":%d", base.port), nil); err != nil {
			fmt.Printf("error: %s\n", err)
		}
	}
}