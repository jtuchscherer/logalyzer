package main

import (
	"container/ring"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path"
	"runtime"
	"time"

	"code.cloudfoundry.org/cli/cf/terminal"
	"code.cloudfoundry.org/cli/cf/trace"
	"code.cloudfoundry.org/cli/plugin"
	"github.com/cloudfoundry/noaa/consumer"
	"github.com/cloudfoundry/sonde-go/events"
	"github.com/gorilla/websocket"
	"github.com/simonleung8/flags"
)

var usage = "cf logalyze APP_NAME --agg-window 2 --buffer-size 100 -p 8080"

// LogalyzerCmd struct for the plugin
type LogalyzerCmd struct {
	ui            terminal.UI
	sendChan      chan []byte
	logCount      int
	errCount      int
	happy         bool
	movingAverage float64
	errorRate     float64
}

// GetMetadata shows metadata for the plugin
func (c *LogalyzerCmd) GetMetadata() plugin.PluginMetadata {
	return plugin.PluginMetadata{
		Name: "logalyzer",
		Version: plugin.VersionType{
			Major: 1,
			Minor: 1,
			Build: 0,
		},
		MinCliVersion: plugin.VersionType{
			Major: 6,
			Minor: 23,
			Build: 0,
		},
		Commands: []plugin.Command{
			{
				Name:     "logalyze",
				HelpText: "Command to analyze the application's log output",
				UsageDetails: plugin.Usage{
					Usage: usage,
				},
			},
		},
	}
}

func main() {
	plugin.Start(new(LogalyzerCmd))
}

// Run will be executed when cf logalyze gets invoked
func (c *LogalyzerCmd) Run(cliConnection plugin.CliConnection, args []string) {
	if args[0] != "logalyze" {
		return
	}

	port := 8080
	aggregationWindow := 2 * time.Second
	bufferSize := 100

	traceLogger := trace.NewLogger(os.Stdout, false, os.Getenv("CF_TRACE"), "")
	c.ui = terminal.NewUI(os.Stdin, os.Stdout, terminal.NewTeePrinter(os.Stdout), traceLogger)

	c.sendChan = make(chan []byte, 256)

	if len(args) < 2 {
		c.ui.Say("Usage: %s\n", usage)
		c.ui.Failed("No App Name given")
		return
	}

	loggedIn, err := cliConnection.IsLoggedIn()
	if err != nil {
		c.ui.Failed(err.Error())
		return
	}

	if !loggedIn {
		c.ui.Failed("You have to be logged in")
		return
	}

	fc := flags.New()
	fc.NewStringFlag("password", "p", "flag for password") //name, short_name and usage of the string flag
	fc.NewIntFlag("buffer-size", "b", "How many past values should be retained for the moving average, default 100")
	fc.NewIntFlag("agg-window", "a", "Over how many seconds should the current error rate be calculated, default 2")
	fc.NewIntFlag("port", "p", "Port to use, default: 8080")

	err = fc.Parse(args[2:]...)
	if err != nil {
		c.ui.Failed(err.Error())
		return
	}

	if fc.IsSet("p") {
		port = fc.Int("p")
	}

	if fc.IsSet("a") {
		aggregationWindow = time.Duration(fc.Int("a")) * time.Second
	}

	if fc.IsSet("b") {
		bufferSize = fc.Int("b")
	}

	appName := args[1]

	dopplerEndpoint, err := cliConnection.DopplerEndpoint()
	if err != nil {
		c.ui.Failed(err.Error())
		return
	}

	appModel, err := cliConnection.GetApp(appName)
	if err != nil {
		c.ui.Failed(err.Error())
		return
	}

	authToken, err := cliConnection.AccessToken()
	if err != nil {
		c.ui.Failed(err.Error())
		return
	}

	isSSLDisabled, err := cliConnection.IsSSLDisabled()
	if err != nil {
		c.ui.Failed(err.Error())
		return
	}

	consumer := consumer.New(dopplerEndpoint, &tls.Config{InsecureSkipVerify: isSSLDisabled}, nil)
	consumer.SetDebugPrinter(ConsoleDebugPrinter{traceLogger})

	outputChan, errorChan := consumer.Stream(appModel.Guid, authToken)

	go c.happyCalcer(aggregationWindow, bufferSize)

	go c.startServer(c.getAssetsDir(), port)
	c.ui.Say("Webserver is started at http://localhost:%d", port)

	for {
		select {
		case log := <-outputChan:
			if log.GetEventType() == events.Envelope_LogMessage {
				if log.GetLogMessage().GetMessageType() == events.LogMessage_ERR {
					c.errCount++
				}
				c.logCount++
				c.sendChan <- log.GetLogMessage().GetMessage()
			}
		case err := <-errorChan:
			c.ui.Failed(err.Error())
		}
	}
}

func (c *LogalyzerCmd) happyCalcer(aggregationWindow time.Duration, bufferSize int) {
	ticker := time.NewTicker(aggregationWindow)

	r := ring.New(bufferSize)
	r.Value = float64(0)
	r = r.Next()

	oldTotal := 0
	oldError := 0
	for {
		select {
		case <-ticker.C:
			currentTotal := c.logCount
			currentError := c.errCount

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
				c.happy = (r.Value.(float64) <= movingAverage)
				c.errorRate = r.Value.(float64)
			}

			c.movingAverage = movingAverage

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

func (c *LogalyzerCmd) serveWs(rw http.ResponseWriter, r *http.Request) {
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
		c.ui.Failed(err.Error())
		return
	}

	conn := &connection{send: c.sendChan, ws: ws}
	conn.writePump()
}

func (c *LogalyzerCmd) startServer(filename string, port int) {
	http.HandleFunc("/ws", c.serveWs)
	http.HandleFunc("/counts", func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte(fmt.Sprintf("{\"logCount\": %d, \"errCount\": %d, \"happy\": %t, \"movingAverage\": %f, \"errorRate\": %f}", c.logCount, c.errCount, c.happy, c.movingAverage, c.errorRate)))
	})

	p := path.Join(path.Dir(filename), "assets")
	http.Handle("/", http.FileServer(http.Dir(p)))

	address := fmt.Sprintf(":%d", port)
	err := http.ListenAndServe(address, nil)
	c.ui.Failed(err.Error())
}

func (c *LogalyzerCmd) getAssetsDir() string {
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

// ConsoleDebugPrinter struct for printing to Console
type ConsoleDebugPrinter struct {
	logger trace.Printer
}

// Print prints logs to the console
func (c ConsoleDebugPrinter) Print(title, dump string) {
	c.logger.Print(title)
	c.logger.Print(dump)
}
