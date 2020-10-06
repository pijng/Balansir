package logutil

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	tagInfo    = "INFO    "
	tagNotice  = "NOTICE  "
	tagWarning = "WARNING "
	tagError   = "ERROR   "
	tagFatal   = "FATAL   "
)

const (
	infoColor    = "\033[1;40m%s\033[0m"
	noticeColor  = "\033[1;32m%s\033[0m"
	warningColor = "\033[1;33m%s\033[0m"
	errorColor   = "\033[1;31m%s\033[0m"
	fatalColor   = "\033[1;35m%s\033[0m"
)

const (
	logDir  = "./log"
	logPath = "./log/balansir.log"
	jsonDir = "./log/.dashboard"
	//JSONPath ...
	JSONPath = "./log/.dashboard/logs.json"
	//StatsPath ...
	StatsPath = "./log/.dashboard/stats.json"
)

//JSONlog ...
type JSONlog struct {
	Timestamp time.Time `json:"timestamp"`
	Tag       string    `json:"tag"`
	Text      string    `json:"text"`
}

//Logger ...
type Logger struct {
	infoLog     *log.Logger
	noticeLog   *log.Logger
	warningLog  *log.Logger
	errorLog    *log.Logger
	fatalLog    *log.Logger
	initialized bool
	mx          sync.RWMutex
}

var defaultLogger *Logger

//Init ...
func Init() {
	if _, err := os.Stat(logDir); os.IsNotExist(err) {
		err := os.Mkdir(logDir, os.ModePerm)
		if err != nil {
			log.Fatalf("failed to create './logs' directory: %v", err)
		}
	}

	if _, err := os.Stat(jsonDir); os.IsNotExist(err) {
		err := os.Mkdir(jsonDir, os.ModePerm)
		if err != nil {
			log.Fatalf("failed to create './logs/.dashboard' directory: %v", err)
		}
	}

	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0660)
	if err != nil {
		log.Fatalf("failed to create/open log file: %v", err)
	}

	_, err = os.OpenFile(JSONPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0660)
	if err != nil {
		log.Fatalf("failed to create/open log file: %v", err)
	}

	_, err = os.OpenFile(StatsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0660)
	if err != nil {
		log.Fatalf("failed to create/open stats file: %v", err)
	}

	iLogs := log.New(io.MultiWriter([]io.Writer{lf}...), "", 0)
	nLogs := log.New(io.MultiWriter([]io.Writer{lf}...), "", 0)
	wLogs := log.New(io.MultiWriter([]io.Writer{lf}...), "", 0)
	eLogs := log.New(io.MultiWriter([]io.Writer{lf}...), "", 0)
	fLogs := log.New(io.MultiWriter([]io.Writer{lf}...), "", 0)

	defaultLogger = &Logger{
		infoLog:     iLogs,
		noticeLog:   nLogs,
		warningLog:  wLogs,
		errorLog:    eLogs,
		fatalLog:    fLogs,
		initialized: true,
	}
}

func (l *Logger) output(severity string, txt string) {
	l.mx.Lock()
	defer l.mx.Unlock()

	switch severity {
	case tagInfo:
		l.infoLog.Output(3, logFormat(infoColor, dateFormat(time.Now()), tagInfo, txt)) //nolint
		l.jsonLogger(time.Now(), tagInfo, txt)

	case tagNotice:
		l.noticeLog.Output(3, logFormat(noticeColor, dateFormat(time.Now()), tagNotice, txt)) //nolint
		l.jsonLogger(time.Now(), tagNotice, txt)

	case tagWarning:
		l.warningLog.Output(3, logFormat(warningColor, dateFormat(time.Now()), tagWarning, txt)) //nolint
		l.jsonLogger(time.Now(), tagWarning, txt)

	case tagError:
		l.errorLog.Output(3, logFormat(errorColor, dateFormat(time.Now()), tagError, txt)) //nolint
		l.jsonLogger(time.Now(), tagError, txt)

	case tagFatal:
		l.fatalLog.Output(3, logFormat(fatalColor, dateFormat(time.Now()), tagFatal, txt)) //nolint
		l.jsonLogger(time.Now(), tagFatal, txt)
	}
}

func logFormat(color string, txt ...string) string {
	return fmt.Sprintf(color, strings.Join(txt, " "))
}

func dateFormat(cTime time.Time) string {
	dateStamp := cTime.Format("2006/01/02")
	timestamp := cTime.Format("15:04:05")

	return fmt.Sprintf("%v %v ", dateStamp, timestamp)
}

func (l *Logger) malformedJSON(err error) {
	l.warningLog.Output(3, logFormat(warningColor, dateFormat(time.Now()), tagWarning, fmt.Sprintf("%s malformed: %v", JSONPath, err))) //nolint
}

func (l *Logger) jsonLogger(cTime time.Time, tag string, txt string) {
	file, err := os.OpenFile(JSONPath, os.O_RDWR, 0644)
	if err != nil {
		l.malformedJSON(err)
		return
	}
	defer file.Close()

	bytes, err := ioutil.ReadAll(file)
	if err != nil {
		l.malformedJSON(err)
		return
	}
	if len(bytes) == 0 {
		bytes = []byte("[]")
	}

	var jsonLogs []JSONlog
	err = json.Unmarshal(bytes, &jsonLogs)
	if err != nil {
		l.malformedJSON(err)
		return
	}

	// trim tag's trailing spaces – we use them in a standard stdout to show logs in a
	// consistent way. On the frontend we use table columns and styles to create that consistency.
	tag = strings.TrimSpace(tag)
	jsonLogs = append(jsonLogs, JSONlog{Timestamp: cTime, Tag: tag, Text: txt})
	newBytes, err := json.Marshal(jsonLogs)
	if err != nil {
		l.malformedJSON(err)
		return
	}

	_, err = file.WriteAt(newBytes, 0)
	if err != nil {
		l.malformedJSON(err)
		return
	}
}

var jsonLogs []byte
var sFile *os.File
var sErr error

func (l *Logger) stats(stats interface{}) {
	l.mx.Lock()
	defer l.mx.Unlock()

	sFile, sErr = os.OpenFile(StatsPath, os.O_RDWR, 0644)
	if sErr != nil {
		l.malformedJSON(sErr)
		return
	}
	defer sFile.Close()

	info, err := sFile.Stat()
	if err != nil {
		l.malformedJSON(err)
		return
	}
	length := info.Size()

	if length == 0 {
		_, err = sFile.WriteAt([]byte("[]"), 0)
		if err != nil {
			Warning(err)
			return
		}
	}

	jsonLogs, err = json.Marshal(stats)
	if err != nil {
		l.malformedJSON(err)
		return
	}

	if length > 2 {
		jsonLogs = append([]byte(","), jsonLogs...)
	}
	jsonLogs = append(jsonLogs, []byte("]")...)

	_, err = sFile.WriteAt(jsonLogs, length-1)
	if err != nil {
		Warning(err)
	}
}

//Info ...
func Info(txt interface{}) {
	defaultLogger.output(tagInfo, fmt.Sprint(txt))
}

//Notice ...
func Notice(txt interface{}) {
	defaultLogger.output(tagNotice, fmt.Sprint(txt))
}

//Warning ...
func Warning(txt interface{}) {
	defaultLogger.output(tagWarning, fmt.Sprint(txt))
}

//Error ...
func Error(txt interface{}) {
	defaultLogger.output(tagError, fmt.Sprint(txt))
}

//Fatal ...
func Fatal(txt interface{}) {
	defaultLogger.output(tagFatal, fmt.Sprint(txt))
}

//Stats ...
func Stats(stats interface{}) {
	defaultLogger.stats(stats)
}
