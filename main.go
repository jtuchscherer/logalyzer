package main

import (
	"github.com/cloudfoundry/cli/plugin"
	"fmt"
	"github.com/cloudfoundry/noaa"
	"crypto/tls"
	"github.com/cloudfoundry/cli/cf/terminal"
	"os"
	"github.com/cloudfoundry/sonde-go/events"
	"time"
	"net/http"
	"runtime"
	"path"
	"container/ring"
	"github.com/gorilla/websocket"
	"github.com/cloudfoundry/cli/flags"
	"github.com/cloudfoundry/cli/flags/flag"
)

type LogalyzerCmd struct {
	ui       terminal.UI
	sendChan chan []byte
	logCount int
	errCount int
	happy    bool
	movingAverage float64
	errorRate float64
}

func setupFlags() map[string]flags.FlagSet {
	fs := make(map[string]flags.FlagSet)
	fs["buffer-size"] = &cliFlags.IntFlag{Name: "buffer-size", Usage: "How many past values should be retained for the moving average, default 100"}
	fs["agg-window"] = &cliFlags.IntFlag{Name: "agg-window", Usage: "Over how many seconds should the current error rate be calculated, default 2"}
	fs["p"] = &cliFlags.IntFlag{Name: "p", Usage: "Port to use, default: 8080"}
	return fs
}

func (c *LogalyzerCmd) GetMetadata() plugin.PluginMetadata {
	return plugin.PluginMetadata{
		Name: "Logalyzer",
		Version: plugin.VersionType{
			Major: 1,
			Minor: 1,
			Build: 0,
		},
		Commands: []plugin.Command{
			{
				Name:     "logalyze",
				HelpText: "Command to analyze the application's log output",
				UsageDetails: plugin.Usage{
					Usage: c.Usage(),
				},
			},
		},
	}
}

func main() {
	plugin.Start(new(LogalyzerCmd))
}

func (cmd *LogalyzerCmd) Usage() string{
	return "cf logalyze APP_NAME --agg-window 2 --buffer-size 100 -p 8080"
}

func (cmd *LogalyzerCmd) Run(cliConnection plugin.CliConnection, args []string) {
	port := 8080
	aggregationWindow := 2 * time.Second
	bufferSize := 100

	cmd.ui = terminal.NewUI(os.Stdin, terminal.NewTeePrinter())
	cmd.sendChan = make(chan []byte, 256)

	if len(args) < 2 {
		cmd.ui.Say("Usage: %s\n", cmd.Usage())
		cmd.ui.Failed("No App Name given")
	}

	fc := flags.NewFlagContext(setupFlags())
	err := fc.Parse(args[2:]...)
	if err != nil {
		cmd.ui.Failed(err.Error())
	}

	if fc.IsSet("p") {
		port = fc.Int("p")
	}

	if fc.IsSet("agg-window") {
		aggregationWindow = time.Duration(fc.Int("agg-window")) * time.Second
	}

	if fc.IsSet("buffer-size") {
		bufferSize = fc.Int("buffer-size")
	}

	appName := args[1]

	dopplerEndpoint, err := cliConnection.DopplerEndpoint()
	if err != nil {
		cmd.ui.Failed(err.Error())
	}

	appModel, err := cliConnection.GetApp(appName)
	if err != nil {
		cmd.ui.Failed(err.Error())
	}

	authToken, err := cliConnection.AccessToken()
	if err != nil {
		cmd.ui.Failed(err.Error())
	}

	outputChan := make(chan *events.LogMessage)
	errorChan := make(chan error)
	dopplerConnection := noaa.NewConsumer(dopplerEndpoint, &tls.Config{InsecureSkipVerify: true}, nil)
	go dopplerConnection.TailingLogs(appModel.Guid, authToken, outputChan, errorChan)

	go cmd.happyCalcer(aggregationWindow, bufferSize)

	go cmd.startServer(cmd.getAssetsDir(), port)
	cmd.ui.Say("Webserver is started at http://localhost:%d", port)

	for {
		select {
		case log := <-outputChan:
			if log.GetMessageType() == events.LogMessage_ERR {
				cmd.errCount++
			}
			cmd.logCount++
			cmd.sendChan <- log.GetMessage()
		case err := <-errorChan:
			cmd.ui.Failed(err.Error())
		}
	}
}

func (cmd *LogalyzerCmd) happyCalcer(aggregationWindow time.Duration, bufferSize int) {
	ticker := time.NewTicker(aggregationWindow)

	r := ring.New(bufferSize)
	r.Value = float64(0)
	r = r.Next()

	oldTotal := 0
	oldError := 0
	for {
		select {
		case <-ticker.C:
			currentTotal := cmd.logCount
			currentError := cmd.errCount

			totalDiff := float64(currentTotal - oldTotal)
			errorDiff := float64(currentError - oldError)

			if totalDiff != 0 {
				r.Value = errorDiff / totalDiff
			} else {
				r.Value = nil
			}


			var movingSum float64
			var numSamples int
			r.Do(func(o interface{}) {
				if o == nil {
					return
				}

				numSamples++
				movingSum += o.(float64)
			})

			var movingAverage float64
			if numSamples != 0 {
				movingAverage = movingSum / float64(numSamples)
			} else {
				movingAverage = 0
			}

			if r.Value != nil {
				cmd.happy = (r.Value.(float64) <= movingAverage)
				cmd.errorRate = r.Value.(float64)
			}

			cmd.movingAverage = movingAverage

			r = r.Next()

			oldTotal = currentTotal
			oldError = currentError
		}
	}
}

type connection struct {
	// The websocket connection.
	ws *websocket.Conn

	// Buffered channel of outbound messages.
	send chan []byte
}

func(cmd *LogalyzerCmd) serveWs(rw http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(rw, "Method not allowed", 405)
		return
	}

	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}

	ws, err := upgrader.Upgrade(rw, r, nil)
	if err != nil {
		cmd.ui.Failed(err.Error())
		return
	}

	conn := &connection{send: cmd.sendChan, ws: ws}
	conn.writePump()
}

func (cmd *LogalyzerCmd) startServer(filename string, port int) {
	http.HandleFunc("/ws", cmd.serveWs)
	http.HandleFunc("/counts", func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte(fmt.Sprintf("{\"logCount\": %d, \"errCount\": %d, \"happy\": %t, \"movingAverage\": %f, \"errorRate\": %f}", cmd.logCount, cmd.errCount, cmd.happy, cmd.movingAverage, cmd.errorRate)))
	})

	p := path.Join(path.Dir(filename), "assets")
	http.Handle("/", http.FileServer(http.Dir(p)))

	address := fmt.Sprintf(":%d", port)
	err := http.ListenAndServe(address, nil)
	cmd.ui.Failed(err.Error())
}

func (cmd *LogalyzerCmd) getAssetsDir() string {
	_, filename, _, _ := runtime.Caller(1)
	return filename
}

func (c *connection) writePump() {
	ticker := time.NewTicker(5 * time.Second)
	defer func() {
		ticker.Stop()
		c.ws.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				c.write(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.write(websocket.TextMessage, message); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.write(websocket.PingMessage, []byte{}); err != nil {
				return
			}
		}
	}
}

func (c *connection) write(mt int, payload []byte) error {
	c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.ws.WriteMessage(mt, payload)
}