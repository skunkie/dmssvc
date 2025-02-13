package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/log"
	"github.com/nfnt/resize"

	"github.com/anacrolix/dms/dlna/dms"
	"github.com/anacrolix/dms/rrcache"
)

//go:embed "data/VGC Sonic.png"
var defaultIcon []byte

type dmsConfig struct {
	Path                string
	IfName              string
	Http                string
	FriendlyName        string
	DeviceIcon          string
	DeviceIconSizes     []string
	LogHeaders          bool
	FFprobeCachePath    string
	NoTranscode         bool
	ForceTranscodeTo    string
	NoProbe             bool
	StallEventSubscribe bool
	NotifyInterval      time.Duration
	IgnoreHidden        bool
	IgnoreUnreadable    bool
	IgnorePaths         []string
	AllowedIpNets       []*net.IPNet
	AllowDynamicStreams bool
	TranscodeLogPattern string
}

func (config *dmsConfig) load(configPath string) error {
	file, err := os.Open(configPath)
	if err != nil {
		return fmt.Errorf("config error (config file: '%s'): %v\n", configPath, err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&config)
	if err != nil {
		return fmt.Errorf("config error: %v\n", err)
	}

	return nil
}

// default config
var config = &dmsConfig{
	Path:             "",
	IfName:           "",
	Http:             ":1338",
	FriendlyName:     "",
	DeviceIcon:       "",
	DeviceIconSizes:  []string{"48,128"},
	LogHeaders:       false,
	FFprobeCachePath: getDefaultFFprobeCachePath(),
	ForceTranscodeTo: "",
	NotifyInterval:   30 * time.Second,
}

func getDefaultFFprobeCachePath() (path string) {
	_user, err := user.Current()
	if err != nil {
		log.Print(err)
		return
	}
	path = filepath.Join(_user.HomeDir, ".dms-ffprobe-cache")
	return
}

type fFprobeCache struct {
	c *rrcache.RRCache
	sync.Mutex
}

func (fc *fFprobeCache) Get(key interface{}) (value interface{}, ok bool) {
	fc.Lock()
	defer fc.Unlock()
	return fc.c.Get(key)
}

func (fc *fFprobeCache) Set(key interface{}, value interface{}) {
	fc.Lock()
	defer fc.Unlock()
	var size int64
	for _, v := range []interface{}{key, value} {
		b, err := json.Marshal(v)
		if err != nil {
			log.Printf("Could not marshal %v: %s", v, err)
			continue
		}
		size += int64(len(b))
	}
	fc.c.Set(key, value, size)
}

func (p *program) run() {
	err := p.runErr()
	if err != nil {
		log.Fatalf("error in runErr: %v", err)
	}
}

func (p *program) runErr() error {
	path := flag.String("path", config.Path, "browse root path")
	ifName := flag.String("ifname", config.IfName, "specific SSDP network interface")
	http := flag.String("http", config.Http, "http server port")
	friendlyName := flag.String("friendlyName", config.FriendlyName, "server friendly name")
	deviceIcon := flag.String("deviceIcon", config.DeviceIcon, "device defaultIcon")
	deviceIconSizes := flag.String("deviceIconSizes", strings.Join(config.DeviceIconSizes, ","), "comma separated list of icon sizes to advertise, eg 48,128,256. Use 48:512,128:512 format to force actual size.")
	logHeaders := flag.Bool("logHeaders", config.LogHeaders, "log HTTP headers")
	fFprobeCachePath := flag.String("fFprobeCachePath", config.FFprobeCachePath, "path to FFprobe cache file")
	configFilePath := flag.String("config", "", "json configuration file")
	allowedIps := flag.String("allowedIps", "", "allowed ip of clients, separated by comma")
	forceTranscodeTo := flag.String("forceTranscodeTo", config.ForceTranscodeTo, "force transcoding to certain format, supported: 'chromecast', 'vp8', 'web'")
	transcodeLogPattern := flag.String("transcodeLogPattern", "", "pattern where to write transcode logs to. The [tsname] placeholder is replaced with the name of the item currently being played. The default is $HOME/.dms/log/[tsname]")
	flag.BoolVar(&config.NoTranscode, "noTranscode", false, "disable transcoding")
	flag.BoolVar(&config.NoProbe, "noProbe", false, "disable media probing with ffprobe")
	flag.BoolVar(&config.StallEventSubscribe, "stallEventSubscribe", false, "workaround for some bad event subscribers")
	flag.DurationVar(&config.NotifyInterval, "notifyInterval", 30*time.Second, "interval between SSPD announces")
	flag.BoolVar(&config.IgnoreHidden, "ignoreHidden", false, "ignore hidden files and directories")
	flag.BoolVar(&config.IgnoreUnreadable, "ignoreUnreadable", false, "ignore unreadable files and directories")
	ignorePaths := flag.String("ignore", "", "comma separated list of directories to ignore (i.e. thumbnails,thumbs)")
	flag.BoolVar(&config.AllowDynamicStreams, "allowDynamicStreams", false, "activate support for dynamic streams described via .dms.json metadata files")

	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
		return fmt.Errorf("%s: %s\n", "unexpected positional arguments", flag.Args())
	}

	config.Path, _ = filepath.Abs(*path)
	config.IfName = *ifName
	config.Http = *http
	config.FriendlyName = *friendlyName
	config.DeviceIcon = *deviceIcon
	config.DeviceIconSizes = strings.Split(*deviceIconSizes, ",")

	config.LogHeaders = *logHeaders
	config.FFprobeCachePath = *fFprobeCachePath
	config.AllowedIpNets = makeIpNets(*allowedIps)
	config.ForceTranscodeTo = *forceTranscodeTo
	config.IgnorePaths = strings.Split(*ignorePaths, ",")
	config.TranscodeLogPattern = *transcodeLogPattern

	if config.FriendlyName == "" {
		config.FriendlyName = func() string {
			name, err := os.Hostname()
			if err != nil {
				return ""
			}

			return fmt.Sprintf("dms on %s", name)
		}()
	}

	if config.TranscodeLogPattern == "" {
		u, err := user.Current()
		if err != nil {
			return fmt.Errorf("unable to resolve current user: %q", err)
		}
		config.TranscodeLogPattern = filepath.Join(u.HomeDir, ".dms", "log", "[tsname]")
	}

	if len(*configFilePath) > 0 {
		config.load(*configFilePath)
	}

	p.logger.Printf("device icon sizes are %q", config.DeviceIconSizes)
	p.logger.Printf("allowed ip nets are %q", config.AllowedIpNets)
	p.logger.Printf("serving folder %q", config.Path)
	if config.AllowDynamicStreams {
		p.logger.Printf("Dynamic streams ARE allowed")
	}

	p.cache = &fFprobeCache{
		c: rrcache.New(64 << 20),
	}
	if err := p.cache.load(config.FFprobeCachePath); err != nil {
		log.Print(err)
	}
	p.dmsServer = p.newDmsServer(getInterfaces(config.IfName))
	if err := p.dmsServer.Init(); err != nil {
		log.Fatalf("error initing dms server: %v", err)
	}
	go func() {
		if err := p.dmsServer.Run(); err != nil {
			log.Fatal(err)
		}
	}()
	go func() {
		for {
			time.Sleep(config.NotifyInterval)
			ifs := getInterfaces(config.IfName)
			if len(p.dmsServer.Interfaces) < len(ifs) {
				if err := p.dmsServer.Close(); err != nil {
					log.Fatalf("error closing dms server: %v", err)
				}
				p.dmsServer = p.newDmsServer(ifs)
				if err := p.dmsServer.Init(); err != nil {
					log.Fatalf("error initing dms server: %v", err)
				}
				go func() {
					if err := p.dmsServer.Run(); err != nil {
						log.Fatal(err)
					}
				}()
			}
		}
	}()

	return nil
}

func (cache *fFprobeCache) load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var items []dms.FfprobeCacheItem
	err = dec.Decode(&items)
	if err != nil {
		return err
	}
	for _, item := range items {
		cache.Set(item.Key, item.Value)
	}
	log.Printf("added %d items from cache", len(items))
	return nil
}

func (cache *fFprobeCache) save(path string) error {
	cache.Lock()
	items := cache.c.Items()
	cache.Unlock()
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	err = enc.Encode(items)
	f.Close()
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	if runtime.GOOS == "windows" {
		err = os.Remove(path)
		if err == os.ErrNotExist {
			err = nil
		}
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err == nil {
		log.Printf("saved cache with %d items", len(items))
	} else {
		os.Remove(f.Name())
	}
	return err
}

func getIconReader(path string) (io.ReadCloser, error) {
	if path == "" {
		return io.NopCloser(bytes.NewReader(defaultIcon)), nil
	}
	return os.Open(path)
}

func readIcon(path string, size uint) []byte {
	r, err := getIconReader(path)
	if err != nil {
		panic(err)
	}
	defer r.Close()
	imageData, _, err := image.Decode(r)
	if err != nil {
		panic(err)
	}
	return resizeImage(imageData, size)
}

func resizeImage(imageData image.Image, size uint) []byte {
	img := resize.Resize(size, size, imageData, resize.Lanczos3)
	var buff bytes.Buffer
	png.Encode(&buff, img)
	return buff.Bytes()
}

func makeIpNets(s string) []*net.IPNet {
	var nets []*net.IPNet
	if len(s) < 1 {
		_, ipnet, _ := net.ParseCIDR("0.0.0.0/0")
		nets = append(nets, ipnet)
		_, ipnet, _ = net.ParseCIDR("::/0")
		nets = append(nets, ipnet)
	} else {
		for _, el := range strings.Split(s, ",") {
			ip := net.ParseIP(el)

			if ip == nil {
				_, ipnet, err := net.ParseCIDR(el)
				if err == nil {
					nets = append(nets, ipnet)
				} else {
					log.Printf("unable to parse expression %q", el)
				}

			} else {
				_, ipnet, err := net.ParseCIDR(el + "/32")
				if err == nil {
					nets = append(nets, ipnet)
				} else {
					log.Printf("unable to parse ip %q", el)
				}
			}
		}
	}
	return nets
}

func (p *program) newDmsServer(ifs []net.Interface) *dms.Server {
	return &dms.Server{
		Logger:     p.logger.WithNames("dms", "server"),
		Interfaces: ifs,
		HTTPConn: func() net.Listener {
			conn, err := net.Listen("tcp", config.Http)
			if err != nil {
				log.Fatal(err)
			}
			return conn
		}(),
		FriendlyName:        config.FriendlyName,
		RootObjectPath:      filepath.Clean(config.Path),
		FFProbeCache:        p.cache,
		LogHeaders:          config.LogHeaders,
		NoTranscode:         config.NoTranscode,
		AllowDynamicStreams: config.AllowDynamicStreams,
		ForceTranscodeTo:    config.ForceTranscodeTo,
		TranscodeLogPattern: config.TranscodeLogPattern,
		NoProbe:             config.NoProbe,
		Icons: func() []dms.Icon {
			var icons []dms.Icon
			for _, size := range config.DeviceIconSizes {
				s := strings.Split(size, ":")
				if len(s) != 1 && len(s) != 2 {
					log.Fatal("bad device icon size: ", size)
				}
				advertisedSize, err := strconv.Atoi(s[0])
				if err != nil {
					log.Fatal("bad device icon size: ", size)
				}
				actualSize := advertisedSize
				if len(s) == 2 {
					// Force actual icon size to be different from advertised
					actualSize, err = strconv.Atoi(s[1])
					if err != nil {
						log.Fatal("bad device icon size: ", size)
					}
				}
				icons = append(icons, dms.Icon{
					Width:    advertisedSize,
					Height:   advertisedSize,
					Depth:    8,
					Mimetype: "image/png",
					Bytes:    readIcon(config.DeviceIcon, uint(actualSize)),
				})
			}
			return icons
		}(),
		StallEventSubscribe: config.StallEventSubscribe,
		NotifyInterval:      config.NotifyInterval,
		IgnoreHidden:        config.IgnoreHidden,
		IgnoreUnreadable:    config.IgnoreUnreadable,
		IgnorePaths:         config.IgnorePaths,
		AllowedIpNets:       config.AllowedIpNets,
	}
}

func getInterfaces(ifName string) (ifs []net.Interface) {
	var err error
	if ifName == "" {
		ifs, err = net.Interfaces()
	} else {
		var if_ *net.Interface
		if_, err = net.InterfaceByName(ifName)
		if if_ != nil {
			ifs = append(ifs, *if_)
		}
	}
	if err != nil {
		log.Fatal(err)
	}
	var tmp []net.Interface
	for _, if_ := range ifs {
		if if_.Flags&net.FlagUp == 0 || if_.MTU <= 0 {
			continue
		}
		tmp = append(tmp, if_)
	}
	ifs = tmp
	return
}
