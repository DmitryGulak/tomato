package main

//go:generate go-bindata -o zbindata.go red.png green.png

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const version = "v1.2.0"

var (
	ModeWork       Mode = "work"
	ModeShortBreak Mode = "short-break"
	ModeLongBreak  Mode = "long-break"

	StateStopped = "[S]"
	StatePaused  = "[P]"
	StateRunning = "[R]"

	N        = 4
	SepColon = ":"
	SepBreak = ":"

	DurationWork       time.Duration
	DurationShortBreak time.Duration
	DurationLongBreak  time.Duration

	Icon1, Icon2, UUID, URL string
	Icon1Data, Icon2Data    string
	Command                 string
	CommandOnStart          string
	CommandAsync            bool

	httpClient = http.Client{Timeout: 200 * time.Millisecond}
)

func main() {
	flag.Usage = func() {
		fmt.Printf(`Tomato on TouchBar %v (works with BetterTouchTool)

Default:
   tomato

With options:
   tomato -n=3 -colon=: -work=25m -short=300s -long=15m -listen=:12321

Send updates to BetterTouchTool:
   tomato -uuid=UUID -port=12345
   tomato -icon1=PATH_ICON1 -icon2=PATH_ICON2 -uuid=UUID -url=http://127.0.0.1:12345/update_touch_bar_widget/

Execute a command at the end of timer:
   tomato -command="terminal-notifier -title Pomodoro -message \"Hey, time is over\!\" -sound default"

Options:
`, version)
		flag.PrintDefaults()
	}

	flListen := flag.String("listen", ":12321", "Address to listen on")

	flag.IntVar(&N, "n", N, "Number of intervals between long break")
	flag.StringVar(&SepColon, "colon", SepColon, "Custom separator")
	flag.StringVar(&SepBreak, "colon-alt", SepBreak, "Alternative separator for break modes")
	flag.StringVar(&Icon1, "icon1", "", "Icon for work (default red)")
	flag.StringVar(&Icon2, "icon2", "", "Icon for break session (default green)")
	flag.StringVar(&Command, "command", "", "Execute command at the end of timer")
	flag.StringVar(&CommandOnStart, "start-command", "", "Execute command on start of timer")
	flag.StringVar(&UUID, "uuid", "", "UUID of the widget")
	flag.BoolVar(&CommandAsync, "async", false, "Execute the command without waiting it to finish (use together with -command)")

	flDurationWork := flag.String("work", "25m", "Work interval")
	flDurationShortBreak := flag.String("short", "5m", "Short break interval")
	flDurationLongBreak := flag.String("long", "15m", "Long break interval")
	flPort := flag.String("port", "", "BetterTouchTool port")
	flURL := flag.String("url", "", "URL to post update")
	flTicker := flag.Int("tick", 100, "Duration in ms for sending updates (default 100)")

	flag.Parse()

	if *flTicker <= 10 || *flTicker >= 1000 {
		fatalf("Invalid ticker value (must between 10 and 1000)")
	}
	if N <= 0 || N >= 10 {
		fatalf("Invalid number of intervals (%v)", N)
	}
	DurationWork = parseDuration(*flDurationWork)
	DurationShortBreak = parseDuration(*flDurationShortBreak)
	DurationLongBreak = parseDuration(*flDurationLongBreak)
	log.Printf("Interval=%v ShortBreak=%v LongBreak=%v N=%v", DurationWork, DurationShortBreak, DurationLongBreak, N)

	switch {
	case *flURL != "" && *flPort != "":
		fatalf("-port and -url can not be used together")
	case (*flPort != "") != (UUID != ""):
		fatalf("-port and -uuid must be used together")
	case *flURL != "":
		URL = *flURL
		if _, err := url.Parse(URL); err != nil {
			fatalf("Unable to parse url: %v", err)
		}
		log.Println("Send update every %vms to URL: %v", *flTicker, URL)
	case *flPort != "":
		URL = fmt.Sprintf("http://127.0.0.1:%v/update_touch_bar_widget/", *flPort)
		log.Printf("Send update every %vms to BetterTouchTool running at :%v with uuid=%v", *flTicker, *flPort, UUID)
	}

	if URL != "" {
		Icon1Data = mustLoadIcon(Icon1, "red.png")
		Icon2Data = mustLoadIcon(Icon2, "green.png")
		err := doRequest(formatTimer(DurationWork, SepColon), Icon1Data)
		if err != nil {
			fatalf("Error while sending request to %v: %v", URL, err)
		}
	}

	s := NewServer()
	go func() {
		ticker := time.NewTicker(time.Duration(*flTicker) * time.Millisecond)
		for _ = range ticker.C {
			s.RefreshStatus(false)
		}
	}()

	if Command != "" {
		async := ""
		if CommandAsync {
			async = " (without waiting it to finish)"
		}
		log.Printf("Command to run at the end of timer%v: %q\n", async, Command)
	}
	log.Printf("Server listen at %v", *flListen)
	err := http.ListenAndServe(*flListen, s.Handler())
	log.Fatal(err)
}

type Mode string

func (mode Mode) Duration() time.Duration {
	switch mode {
	case ModeWork:
		return DurationWork
	case ModeShortBreak:
		return DurationShortBreak
	case ModeLongBreak:
		return DurationLongBreak
	}
	panic("unexpected")
}

func (mode Mode) Sep() string {
	switch mode {
	case ModeWork:
		return SepColon
	case ModeShortBreak, ModeLongBreak:
		return SepBreak
	}
	panic("unexpected")
}

type Server struct {
	mode  Mode
	state string
	t     time.Time
	d     time.Duration // remaining duration
	count int
}

func NewServer() *Server {
	return &Server{
		mode:  ModeWork,
		state: StateStopped,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.Index)
	mux.HandleFunc("/status", s.Status)
	mux.HandleFunc("/time", s.Time)
	mux.HandleFunc("/action/start", s.ActionStart)
	mux.HandleFunc("/action/stop", s.ActionStop)

	return mux
}

func (s *Server) Index(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.NotFound(w, r)
		return
	}

	fmt.Fprintf(w, "Tomato %v\n", version)
}

func (s *Server) Status(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.NotFound(w, r)
		return
	}

	str := s.RefreshStatus(true)
	if r.Header.Get("Accept") == "application/json" {
		w.Write(s.formatStatusJSON())
	} else {
		fmt.Fprint(w, str)
	}
}

func (s *Server) Time(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.NotFound(w, r)
		return
	}

	str := s.RefreshStatus(true)
	fmt.Fprint(w, str)
}

// ActionStart starts or pauses the current interval.
func (s *Server) ActionStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.NotFound(w, r)
		return
	}

	now := time.Now()
	switch s.state {
	case StateStopped:
		t := now.Add(s.mode.Duration())
		s.t = t
		s.state = StateRunning
		s.executeCommandOnStart()

	case StatePaused:
		t := now.Add(s.d)
		s.t = t
		s.state = StateRunning
		s.executeCommandOnStart()

	case StateRunning:
		s.RefreshStatus(true)
		if s.state == StateRunning {
			s.d = s.t.Sub(now)
			s.state = StatePaused
		}
	}

	str := s.formatTimer()
	fmt.Fprint(w, str)
}

// ActionStop stops the current running interval or switch mode.
func (s *Server) ActionStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.NotFound(w, r)
		return
	}

	switch s.state {
	case StateRunning, StatePaused:
		s.state = StateStopped
	case StateStopped:
		switch s.mode {
		case ModeWork:
			if s.count < N {
				s.mode = ModeShortBreak
			} else {
				s.mode = ModeLongBreak
			}

		case ModeShortBreak:
			s.mode = ModeWork

		case ModeLongBreak:
			s.mode = ModeShortBreak
		}
	}

	str := s.RefreshStatus(true)
	fmt.Fprint(w, str)
}

func (s *Server) nextMode() {
	switch s.mode {
	case ModeShortBreak, ModeLongBreak:
		if s.mode == ModeLongBreak {
			s.count = 0
		}

		s.mode = ModeWork

	case ModeWork:
		s.count++
		if s.count < N {
			s.mode = ModeShortBreak
		} else {
			s.mode = ModeLongBreak
		}

	default:
		panic("unexpected")
	}
}

func (s *Server) executeCommand() {
	if Command == "" {
		return
	}

	cmd := exec.Command("/bin/sh", "-c", Command)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	var err error
	if CommandAsync {
		log.Println("Executing command (without waiting it to finish)...")
		err = cmd.Start()
		if err == nil {
			go func() {
				err2 := cmd.Wait()
				if err2 != nil {
					printCommandError(err)
				} else {
					log.Println("Command executed")
				}
			}()
		}
	} else {
		err = cmd.Run()
		if err == nil {
			log.Println("Command executed")
		}
	}
	if err != nil {
		printCommandError(err)
	}
}

func (s *Server) executeCommandOnStart() {
	if CommandOnStart == "" {
		return
	}

	cmd := exec.Command("/bin/sh", "-c", CommandOnStart)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	var err error
	if CommandAsync {
		log.Println("Executing command (without waiting it to finish)...")
		err = cmd.Start()
		if err == nil {
			go func() {
				err2 := cmd.Wait()
				if err2 != nil {
					printCommandError(err)
				} else {
					log.Println("Command executed")
				}
			}()
		}
	} else {
		err = cmd.Run()
		if err == nil {
			log.Println("Command executed")
		}
	}
	if err != nil {
		printCommandError(err)
	}
}

func printCommandError(err error) {
	log.Println("Failed to execute command at end of timer:", err)

	if strings.Contains(err.Error(), "exit status 127") &&
		strings.Contains(Command, "terminal-notifier") {
		log.Println("Note: You may need to download terminal-notifier at https://github.com/julienXX/terminal-notifier")
	}
}

func (s *Server) RefreshStatus(output bool) string {
	switch s.state {
	case StateRunning:
		if time.Now().After(s.t) {
			s.state = StateStopped
			s.nextMode()
			s.executeCommand()
			output = true
		}
	}
	return s.outputStatus(output)
}

func (s *Server) formatStatusJSON() []byte {
	data, _ := json.Marshal(map[string]interface{}{
		"mode":  s.mode,
		"state": s.state,
		"timer": s.formatTimer(),
		"i":     s.count,
		"n":     N,
	})
	return data
}

func (s *Server) formatStatus() string {
	return fmt.Sprintf("%v %v %d/%d %v", s.state, s.formatTimer(), s.count, N, s.mode)
}

func (s *Server) formatTimer() string {
	switch s.state {
	case StateStopped:
		return formatTimer(s.mode.Duration(), s.mode.Sep())
	case StatePaused:
		return formatTimer(s.d, s.mode.Sep())
	case StateRunning:
		return formatTimer(s.t.Sub(time.Now()), s.mode.Sep())
	}
	panic("unexpected")
}

func (s *Server) outputStatus(output bool) string {
	if output {
		log.Print(s.formatStatus())
	}
	str := s.formatTimer()
	if URL != "" {
		iconData := Icon1Data
		if s.mode != ModeWork {
			iconData = Icon2Data
		}
		go func() {
			err := doRequest(str, iconData)
			if err != nil {
				log.Printf("Error while sending request: %v", err)
			}
		}()
	}
	return str
}

func fatalf(format string, args ...interface{}) {
	fmt.Printf(format, args...)
	fmt.Println()
	fmt.Println("Execute `tomato -help` for usage.")
	os.Exit(1)
}

func formatTimer(d time.Duration, sep string) string {
	if d < 0 {
		d = 0
	}
	m := int(d / time.Minute)
	s := int((d % time.Minute) / time.Second)
	if m > 99 {
		m = 99
	}

	return fmt.Sprintf("%02d%s%02d", m, sep, s)
}

func parseDuration(s string) time.Duration {
	if s == "" {
		fatalf("Invalid duration `%v`", s)
	}
	unit := time.Minute
	switch s[len(s)-1] {
	case 'm':
		s = s[:len(s)-1]
	case 's':
		unit = time.Second
		s = s[:len(s)-1]
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		fatalf("Invalid duration `%v`", s)
	}

	if i <= 0 {
		fatalf("Invalid duration `%v`", s)
	}
	return time.Duration(i) * unit
}

func mustLoad(data []byte, err error) []byte {
	if err != nil {
		fatalf("Unable to load icon: %v", err)
	}
	return data
}

func mustLoadIcon(filename, defaultIcon string) string {
	var data []byte
	if filename == "" {
		data = mustLoad(Asset(defaultIcon))
	} else {
		data = mustLoad(ioutil.ReadFile(filename))
	}
	return base64.StdEncoding.EncodeToString(data)
}

var lastText, lastIcon string

func doRequest(text string, iconData string) error {
	if text == lastText && iconData == lastIcon {
		return nil
	}
	defer func() {
		lastText = text
		lastIcon = iconData
	}()

	u, err := url.Parse(URL)
	if err != nil {
		panic(err)
	}
	q := u.Query()
	q.Set("uuid", UUID)
	q.Set("text", text)
	q.Set("icon_data", iconData)
	u.RawQuery = q.Encode()

	resp, err := httpClient.Get(u.String())
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Response status: %v", resp.Status)
	}
	return nil
}
