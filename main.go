package main

import (
	"bitbucket.org/anacrolix/dms/dlna"
	"bitbucket.org/anacrolix/dms/ffmpeg"
	"bitbucket.org/anacrolix/dms/futures"
	"bitbucket.org/anacrolix/dms/misc"
	"bitbucket.org/anacrolix/dms/soap"
	"bitbucket.org/anacrolix/dms/ssdp"
	"bitbucket.org/anacrolix/dms/upnp"
	"bitbucket.org/anacrolix/dms/upnpav"
	"bytes"
	"crypto/md5"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	serverField             = "Linux/3.4 DLNADOC/1.50 UPnP/1.0 DMS/1.0"
	rootDeviceType          = "urn:schemas-upnp-org:device:MediaServer:1"
	rootDeviceModelName     = "dms 1.0"
	resPath                 = "/res"
	rootDescPath            = "/rootDesc.xml"
	ContentDirectorySCPDURL = "/scpd/ContentDirectory.xml"
)

func makeDeviceUuid() string {
	h := md5.New()
	if _, err := io.WriteString(h, friendlyName); err != nil {
		panic(err)
	}
	buf := h.Sum(nil)
	return fmt.Sprintf("uuid:%x-%x-%x-%x-%x", buf[:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

var services = []upnp.Service{
	upnp.Service{
		ServiceType: "urn:schemas-upnp-org:service:ContentDirectory:1",
		ServiceId:   "urn:upnp-org:serviceId:ContentDirectory",
		SCPDURL:     ContentDirectorySCPDURL,
		ControlURL:  "/ctl/ContentDirectory",
	},
	/*
		service{
			ServiceType: "urn:schemas-upnp-org:service:ConnectionManager:1",
			ServiceId:   "urn:upnp-org:serviceId:ConnectionManager",
			SCPDURL:     "/scpd/ConnectionManager.xml",
			ControlURL:  "/ctl/ConnectionManager",
		},
	*/
}

func devices() []string {
	return []string{
		"urn:schemas-upnp-org:device:MediaServer:1",
	}
}

func serviceTypes() (ret []string) {
	for _, s := range services {
		ret = append(ret, s.ServiceType)
	}
	return
}
func httpPort() int {
	return httpConn.Addr().(*net.TCPAddr).Port
}

func serveHTTP() {
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Ext", "")
			w.Header().Set("Server", serverField)
			http.DefaultServeMux.ServeHTTP(w, r)
		}),
	}
	err := srv.Serve(httpConn)
	log.Fatalln(err)
}

func doSSDP() {
	active := 0
	stopped := make(chan struct{})
	ifs, err := net.Interfaces()
	if err != nil {
		log.Fatalln("error enumerating interfaces:", err)
	}
	for _, if_ := range ifs {
		s := ssdp.Server{
			Interface:      if_,
			Devices:        devices(),
			Services:       serviceTypes(),
			Location:       location,
			Server:         serverField,
			UUID:           rootDeviceUUID,
			NotifyInterval: 30,
		}
		active++
		go func(if_ net.Interface) {
			if err := s.Init(); err != nil {
				log.Println(if_.Name, err)
			} else {
				log.Println("started SSDP on", if_.Name)
				if err := s.Serve(); err != nil {
					log.Println(if_.Name, err)
				}
				s.Close()
			}
			stopped <- struct{}{}
		}(if_)
	}
	for active > 0 {
		<-stopped
		active--
	}
	log.Fatalln("no interfaces remain")
}

var (
	startTime      time.Time
	rootDeviceUUID string
	httpConn       *net.TCPListener
	rootDescXML    []byte
	rootObjectPath string
	friendlyName   string
)

func childCount(path_ string) int {
	f, err := os.Open(path_)
	if err != nil {
		log.Println(err)
		return 0
	}
	defer f.Close()
	fis, err := f.Readdir(-1)
	if err != nil {
		log.Println(err)
		return 0
	}
	ret := 0
	for _, fi := range fis {
		ret += len(fileEntries(fi, path_))
	}
	return ret
}

func itemResExtra(path string) (bitrate uint, duration string) {
	t := time.Now()
	info, err := ffmpeg.Probe(path)
	log.Println("probe took", time.Now().Sub(t))
	if err != nil {
		log.Printf("error probing %s: %s", path, err)
		return
	}
	fmt.Sscan(info.Format["bit_rate"], &bitrate)
	if d := info.Format["duration"]; d != "N/A" {
		var f float64
		_, err = fmt.Sscan(info.Format["duration"], &f)
		if err != nil {
			log.Printf("probed duration for %s: %s\n", path, err)
		} else {
			duration = misc.FormatDurationSexagesimal(time.Duration(f * float64(time.Second)))
		}
	}
	return
}

func entryObject(parentID, host string, entry CDSEntry) interface{} {
	path_ := path.Join(entry.ParentPath, entry.FileInfo.Name())
	obj := upnpav.Object{
		ID:         path_,
		Title:      entry.Title,
		Restricted: 1,
		ParentID:   parentID,
	}
	if entry.FileInfo.IsDir() {
		obj.Class = "object.container.storageFolder"
		return upnpav.Container{
			Object:     obj,
			ChildCount: childCount(path_),
		}
	}
	mimeType := func() string {
		if entry.Transcode {
			return "video/mpeg"
		}
		return mime.TypeByExtension(path.Ext(entry.FileInfo.Name()))
	}()
	obj.Class = "object.item." + strings.SplitN(mimeType, "/", 2)[0] + "Item"
	values := url.Values{}
	values.Set("path", path_)
	if entry.Transcode {
		values.Set("transcode", "t")
	}
	url_ := &url.URL{
		Scheme:   "http",
		Host:     host,
		Path:     resPath,
		RawQuery: values.Encode(),
	}
	cf := dlna.ContentFeatures{}
	if entry.Transcode {
		cf.SupportTimeSeek = true
		cf.Transcoded = true
	} else {
		cf.SupportRange = true
	}
	mainRes := upnpav.Resource{
		ProtocolInfo: "http-get:*:" + mimeType + ":" + cf.String(),
		URL:          url_.String(),
		Size:         uint64(entry.FileInfo.Size()),
	}
	mainRes.Bitrate, mainRes.Duration = itemResExtra(path_)
	return upnpav.Item{
		Object: obj,
		Res:    []upnpav.Resource{mainRes},
	}
}

func fileEntries(fileInfo os.FileInfo, parentPath string) []CDSEntry {
	if fileInfo.IsDir() {
		return []CDSEntry{
			{fileInfo.Name(), fileInfo, parentPath, false},
		}
	}
	mimeType := mime.TypeByExtension(path.Ext(fileInfo.Name()))
	mimeTypeType := strings.SplitN(mimeType, "/", 2)[0]
	ret := []CDSEntry{
		{fileInfo.Name(), fileInfo, parentPath, false},
	}
	if mimeTypeType == "video" {
		ret = append(ret, CDSEntry{
			fileInfo.Name() + "/transcode",
			fileInfo,
			parentPath,
			true,
		})
	}
	return ret
}

type CDSEntry struct {
	Title      string
	FileInfo   os.FileInfo
	ParentPath string
	Transcode  bool
}

type FileInfoSlice []os.FileInfo

func (me FileInfoSlice) Len() int {
	return len(me)
}

func (me FileInfoSlice) Less(i, j int) bool {
	return strings.ToLower(me[i].Name()) < strings.ToLower(me[j].Name())
}

func (me FileInfoSlice) Swap(i, j int) {
	me[i], me[j] = me[j], me[i]
}

func ReadContainer(path_, parentID, host string) (ret []interface{}) {
	dir, err := os.Open(path_)
	if err != nil {
		log.Println(err)
		return
	}
	defer dir.Close()
	var fis FileInfoSlice
	fis, err = dir.Readdir(-1)
	if err != nil {
		panic(err)
	}
	sort.Sort(fis)
	pool := futures.NewExecutor(runtime.NumCPU())
	defer pool.Shutdown()
	for obj := range pool.Map(func(entry futures.I) futures.R {
		return entryObject(parentID, host, entry.(CDSEntry))
	}, func() <-chan futures.I {
		ret := make(chan futures.I)
		go func() {
			for _, fi := range fis {
				for _, entry := range fileEntries(fi, path_) {
					ret <- entry
				}
			}
			close(ret)
		}()
		return ret
	}()) {
		ret = append(ret, obj)
	}
	return
}

type Browse struct {
	ObjectID       string
	BrowseFlag     string
	Filter         string
	StartingIndex  int
	RequestedCount int
}

func contentDirectoryResponseArgs(sa upnp.SoapAction, argsXML []byte, host string) (map[string]string, *upnp.Error) {
	switch sa.Action {
	case "GetSortCapabilities":
		return map[string]string{
			"SortCaps": "dc:title",
		}, nil
	case "Browse":
		var browse Browse
		if err := xml.Unmarshal([]byte(argsXML), &browse); err != nil {
			panic(err)
		}
		path := browse.ObjectID
		if path == "0" {
			path = rootObjectPath
		}
		switch browse.BrowseFlag {
		case "BrowseDirectChildren":
			objs := ReadContainer(path, browse.ObjectID, host)
			totalMatches := len(objs)
			objs = objs[browse.StartingIndex:]
			if browse.RequestedCount != 0 && int(browse.RequestedCount) < len(objs) {
				objs = objs[:browse.RequestedCount]
			}
			result, err := xml.MarshalIndent(objs, "", "  ")
			if err != nil {
				panic(err)
			}
			return map[string]string{
				"TotalMatches":   fmt.Sprint(totalMatches),
				"NumberReturned": fmt.Sprint(len(objs)),
				"Result":         didl_lite(string(result)),
				"UpdateID":       "0",
			}, nil
		default:
			log.Println("unhandled browse flag:", browse.BrowseFlag)
		}
	case "GetSearchCapabilities":
		return map[string]string{
			"SearchCaps": "",
		}, nil
	}
	log.Println("unhandled content directory action:", sa.Action)
	return nil, &upnp.InvalidActionError
}

func serveDLNATranscode(w http.ResponseWriter, r *http.Request, path_ string) {
	w.Header().Set(dlna.TransferModeDomain, "Streaming")
	w.Header().Set("content-type", "video/mpeg")
	w.Header().Set(dlna.ContentFeaturesDomain, (dlna.ContentFeatures{
		Transcoded:      true,
		SupportTimeSeek: true,
	}).String())
	dlnaRangeHeader := r.Header.Get(dlna.TimeSeekRangeDomain)
	var nptStart, nptLength string
	if dlnaRangeHeader != "" {
		if !strings.HasPrefix(dlnaRangeHeader, "npt=") {
			log.Println("bad range:", dlnaRangeHeader)
			return
		}
		dlnaRange, err := dlna.ParseNPTRange(dlnaRangeHeader[len("npt="):])
		if err != nil {
			log.Println("bad range:", dlnaRangeHeader)
			return
		}
		nptStart = dlna.FormatNPTTime(dlnaRange.Start)
		if dlnaRange.End > 0 {
			nptLength = dlna.FormatNPTTime(dlnaRange.End - dlnaRange.Start)
		}
		// passing an NPT duration seems to cause trouble
		// so just pass the "iono" duration
		w.Header().Set(dlna.TimeSeekRangeDomain, dlnaRangeHeader+"/*")
	}
	p, err := misc.Transcode(path_, nptStart, nptLength)
	if err != nil {
		log.Println(err)
		return
	}
	defer p.Close()
	w.WriteHeader(206)
	if n, err := io.Copy(w, p); err != nil {
		// not an error if the peer closes the connection
		func() {
			if opErr, ok := err.(*net.OpError); ok {
				if opErr.Err == syscall.EPIPE {
					return
				}
			}
			log.Println("after copying", n, "bytes:", err)
		}()
	}
}

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)
	startTime = time.Now()
	friendlyName = fmt.Sprintf("%s: %s on %s", rootDeviceModelName, func() string {
		user, err := user.Current()
		if err != nil {
			panic(err)
		}
		return user.Name
	}(), func() string {
		name, err := os.Hostname()
		if err != nil {
			panic(err)
		}
		return name
	}())
	rootDeviceUUID = makeDeviceUuid()
	flag.StringVar(&rootObjectPath, "path", ".", "browse root path")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "%s: %s\n", "unexpected positional arguments", flag.Args())
		flag.Usage()
		os.Exit(2)
	}
	var err error
	rootDescXML, err = xml.MarshalIndent(
		upnp.DeviceDesc{
			SpecVersion: upnp.SpecVersion{Major: 1, Minor: 0},
			Device: upnp.Device{
				DeviceType:   rootDeviceType,
				FriendlyName: friendlyName,
				Manufacturer: "Matt Joiner <anacrolix@gmail.com>",
				ModelName:    rootDeviceModelName,
				UDN:          rootDeviceUUID,
				ServiceList:  services,
			},
		},
		" ", "  ")
	if err != nil {
		panic(err)
	}
	rootDescXML = append([]byte(`<?xml version="1.0"?>`), rootDescXML...)
	httpConn, err = net.ListenTCP("tcp", &net.TCPAddr{Port: 1338})
	if err != nil {
		log.Fatalln(err)
	}
	defer httpConn.Close()
	log.Println("HTTP server on", httpConn.Addr())
	http.HandleFunc("/", func(http.ResponseWriter, *http.Request) {
		panic(nil)
	})
	http.HandleFunc(resPath, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			log.Println(err)
		}
		path := r.Form.Get("path")
		if r.Form.Get("transcode") == "" {
			log.Printf("serving file%s: %s\n", func() (s string) {
				s = r.Header.Get("Range")
				if s != "" {
					s = "(Range: " + s + ")"
				}
				return
			}(), path)
			http.ServeFile(w, r, path)
			return
		}
		serveDLNATranscode(w, r, path)
	})
	http.HandleFunc(rootDescPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", `text/xml; charset="utf-8"`)
		w.Header().Set("content-length", fmt.Sprint(len(rootDescXML)))
		w.Header().Set("server", serverField)
		w.Write(rootDescXML)
	})
	http.HandleFunc(ContentDirectorySCPDURL, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", `text/xml; charset="utf-8"`)
		http.ServeContent(w, r, ".xml", startTime, bytes.NewReader([]byte(ContentDirectoryServiceDescription)))
	})
	http.HandleFunc("/ctl/ContentDirectory", func(w http.ResponseWriter, r *http.Request) {
		soapActionString := r.Header.Get("SOAPACTION")
		soapAction, ok := upnp.ParseActionHTTPHeader(soapActionString)
		if !ok {
			log.Println("invalid soapaction:", soapActionString)
			return
		}
		log.Printf("SOAP request from %s: %s\n", r.RemoteAddr, soapAction)
		var env soap.Envelope
		if err := xml.NewDecoder(r.Body).Decode(&env); err != nil {
			panic(err)
		}
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		w.Header().Set("Ext", "")
		w.Header().Set("Server", serverField)
		actionResponseXML, err := xml.MarshalIndent(func() interface{} {
			argMap, err := contentDirectoryResponseArgs(soapAction, env.Body.Action, r.Host)
			if err != nil {
				w.WriteHeader(500)
				return soap.NewFault("UPnPError", err)
			}
			args := make([]soap.Arg, 0, len(argMap))
			for argName, value := range argMap {
				args = append(args, soap.Arg{
					xml.Name{Local: argName},
					value,
				})
			}
			return soap.Action{
				xml.Name{
					Space: soapAction.ServiceURN.String(),
					Local: soapAction.Action + "Response",
				},
				args,
			}
		}(), "", "  ")
		if err != nil {
			panic(err)
		}
		body, err := xml.MarshalIndent(soap.NewEnvelope(actionResponseXML), "", "  ")
		if err != nil {
			panic(err)
		}
		body = append([]byte(xml.Header), body...)
		if _, err := w.Write(body); err != nil {
			panic(err)
		}
	})
	go serveHTTP()
	doSSDP()
}

func didl_lite(chardata string) string {
	return `<DIDL-Lite` +
		` xmlns:dc="http://purl.org/dc/elements/1.1/"` +
		` xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/"` +
		` xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/"` +
		` xmlns:dlna="urn:schemas-dlna-org:metadata-1-0/">` +
		chardata +
		`</DIDL-Lite>`
}

func location(ip net.IP) string {
	url := url.URL{
		Scheme: "http",
		Host: (&net.TCPAddr{
			IP:   ip,
			Port: httpPort(),
		}).String(),
		Path: rootDescPath,
	}
	return url.String()
}

const ContentDirectoryServiceDescription = `<?xml version="1.0"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0">
  <specVersion>
    <major>1</major>
    <minor>0</minor>
  </specVersion>
  <actionList>
    <action>
      <name>GetSearchCapabilities</name>
      <argumentList>
        <argument>
          <name>SearchCaps</name>
          <direction>out</direction>
          <relatedStateVariable>SearchCapabilities</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>GetSortCapabilities</name>
      <argumentList>
        <argument>
          <name>SortCaps</name>
          <direction>out</direction>
          <relatedStateVariable>SortCapabilities</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>GetSortExtensionCapabilities</name>
      <argumentList>
        <argument>
          <name>SortExtensionCaps</name>
          <direction>out</direction>
          <relatedStateVariable>SortExtensionCapabilities</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>GetFeatureList</name>
      <argumentList>
        <argument>
          <name>FeatureList</name>
          <direction>out</direction>
          <relatedStateVariable>FeatureList</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>GetSystemUpdateID</name>
      <argumentList>
        <argument>
          <name>Id</name>
          <direction>out</direction>
          <relatedStateVariable>SystemUpdateID</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>Browse</name>
      <argumentList>
        <argument>
          <name>ObjectID</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable>
        </argument>
        <argument>
          <name>BrowseFlag</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_BrowseFlag</relatedStateVariable>
        </argument>
        <argument>
          <name>Filter</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_Filter</relatedStateVariable>
        </argument>
        <argument>
          <name>StartingIndex</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_Index</relatedStateVariable>
        </argument>
        <argument>
          <name>RequestedCount</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable>
        </argument>
        <argument>
          <name>SortCriteria</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_SortCriteria</relatedStateVariable>
        </argument>
        <argument>
          <name>Result</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_Result</relatedStateVariable>
        </argument>
        <argument>
          <name>NumberReturned</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable>
        </argument>
        <argument>
          <name>TotalMatches</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable>
        </argument>
        <argument>
          <name>UpdateID</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_UpdateID</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>Search</name>
      <argumentList>
        <argument>
          <name>ContainerID</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable>
        </argument>
        <argument>
          <name>SearchCriteria</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_SearchCriteria</relatedStateVariable>
        </argument>
        <argument>
          <name>Filter</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_Filter</relatedStateVariable>
        </argument>
        <argument>
          <name>StartingIndex</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_Index</relatedStateVariable>
        </argument>
        <argument>
          <name>RequestedCount</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable>
        </argument>
        <argument>
          <name>SortCriteria</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_SortCriteria</relatedStateVariable>
        </argument>
        <argument>
          <name>Result</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_Result</relatedStateVariable>
        </argument>
        <argument>
          <name>NumberReturned</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable>
        </argument>
        <argument>
          <name>TotalMatches</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable>
        </argument>
        <argument>
          <name>UpdateID</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_UpdateID</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>CreateObject</name>
      <argumentList>
        <argument>
          <name>ContainerID</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable>
        </argument>
        <argument>
          <name>Elements</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_Result</relatedStateVariable>
        </argument>
        <argument>
          <name>ObjectID</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable>
        </argument>
        <argument>
          <name>Result</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_Result</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>DestroyObject</name>
      <argumentList>
        <argument>
          <name>ObjectID</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>UpdateObject</name>
      <argumentList>
        <argument>
          <name>ObjectID</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable>
        </argument>
        <argument>
          <name>CurrentTagValue</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_TagValueList</relatedStateVariable>
        </argument>
        <argument>
          <name>NewTagValue</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_TagValueList</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>MoveObject</name>
      <argumentList>
        <argument>
          <name>ObjectID</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable>
        </argument>
        <argument>
          <name>NewParentID</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable>
        </argument>
        <argument>
          <name>NewObjectID</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>ImportResource</name>
      <argumentList>
        <argument>
          <name>SourceURI</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_URI</relatedStateVariable>
        </argument>
        <argument>
          <name>DestinationURI</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_URI</relatedStateVariable>
        </argument>
        <argument>
          <name>TransferID</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_TransferID</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>ExportResource</name>
      <argumentList>
        <argument>
          <name>SourceURI</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_URI</relatedStateVariable>
        </argument>
        <argument>
          <name>DestinationURI</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_URI</relatedStateVariable>
        </argument>
        <argument>
          <name>TransferID</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_TransferID</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>StopTransferResource</name>
      <argumentList>
        <argument>
          <name>TransferID</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_TransferID</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>DeleteResource</name>
      <argumentList>
        <argument>
          <name>ResourceURI</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_URI</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>GetTransferProgress</name>
      <argumentList>
        <argument>
          <name>TransferID</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_TransferID</relatedStateVariable>
        </argument>
        <argument>
          <name>TransferStatus</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_TransferStatus</relatedStateVariable>
        </argument>
        <argument>
          <name>TransferLength</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_TransferLength</relatedStateVariable>
        </argument>
        <argument>
          <name>TransferTotal</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_TransferTotal</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
    <action>
      <name>CreateReference</name>
      <argumentList>
        <argument>
          <name>ContainerID</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable>
        </argument>
        <argument>
          <name>ObjectID</name>
          <direction>in</direction>
          <relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable>
        </argument>
        <argument>
          <name>NewID</name>
          <direction>out</direction>
          <relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable>
        </argument>
      </argumentList>
    </action>
  </actionList>
  <serviceStateTable>
    <stateVariable sendEvents="no">
      <name>SearchCapabilities</name>
      <dataType>string</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>SortCapabilities</name>
      <dataType>string</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>SortExtensionCapabilities</name>
      <dataType>string</dataType>
    </stateVariable>
    <stateVariable sendEvents="yes">
      <name>SystemUpdateID</name>
      <dataType>ui4</dataType>
    </stateVariable>
    <stateVariable sendEvents="yes">
      <name>ContainerUpdateIDs</name>
      <dataType>string</dataType>
    </stateVariable>
    <stateVariable sendEvents="yes">
      <name>TransferIDs</name>
      <dataType>string</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>FeatureList</name>
      <dataType>string</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>A_ARG_TYPE_ObjectID</name>
      <dataType>string</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>A_ARG_TYPE_Result</name>
      <dataType>string</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>A_ARG_TYPE_SearchCriteria</name>
      <dataType>string</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>A_ARG_TYPE_BrowseFlag</name>
      <dataType>string</dataType>
      <allowedValueList>
        <allowedValue>BrowseMetadata</allowedValue>
        <allowedValue>BrowseDirectChildren</allowedValue>
      </allowedValueList>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>A_ARG_TYPE_Filter</name>
      <dataType>string</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>A_ARG_TYPE_SortCriteria</name>
      <dataType>string</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>A_ARG_TYPE_Index</name>
      <dataType>ui4</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>A_ARG_TYPE_Count</name>
      <dataType>ui4</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>A_ARG_TYPE_UpdateID</name>
      <dataType>ui4</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>A_ARG_TYPE_TransferID</name>
      <dataType>ui4</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>A_ARG_TYPE_TransferStatus</name>
      <dataType>string</dataType>
      <allowedValueList>
        <allowedValue>COMPLETED</allowedValue>
        <allowedValue>ERROR</allowedValue>
        <allowedValue>IN_PROGRESS</allowedValue>
        <allowedValue>STOPPED</allowedValue>
      </allowedValueList>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>A_ARG_TYPE_TransferLength</name>
      <dataType>string</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>A_ARG_TYPE_TransferTotal</name>
      <dataType>string</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>A_ARG_TYPE_TagValueList</name>
      <dataType>string</dataType>
    </stateVariable>
    <stateVariable sendEvents="no">
      <name>A_ARG_TYPE_URI</name>
      <dataType>uri</dataType>
    </stateVariable>
  </serviceStateTable>
</scpd>`
