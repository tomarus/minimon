// Hate to all monitoring systems.
package main

import (
	"bytes"
	"crypto/md5"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/smtp"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/garyburd/redigo/redis"
)

type Global struct {
	Redis   string `json:"redis"`
	RedisDB int    `json:"redisdb"`
}

type SMTP struct {
	Server   string `json:"server"`
	Addr     string `json:"addr"`
	User     string `json:"user"`
	Password string `json:"pass"`
}

type Command struct {
	Command string `json:"command"`
	Args    string `json:"args"`
}

type Alarm struct {
	Type string `json:"type"`
	Args string `json:"args"`
}

type Check struct {
	Id        string   `json:"id"`
	Args      []string `json:"args,omitempty"`
	Checklist string   `json:"checklist"`
	Command   string   `json:"command"`
	Alarms    []string `json:"alarms"`
	Schedule  string   `json:"schedule"`
	Report    string   `json:"report"`
	Errors    int64    `json:"errors"`
}

type Stats struct {
	Id         string  `redis:"-" json:"id"`
	CheckId    string  `redis:"-" json:"checkid"`
	Command    string  `redis:"-" json:"command"`
	LastCheck  int64   `redis:"lastcheck" json:"lastcheck"`
	LastOk     int64   `redis:"lastok" json:"lastok"`
	CheckTime  float32 `redis:"checktime" json:"checktime"`
	Stdout     string  `redis:"stdout" json:"stdout"`
	Stderr     string  `redis:"stderr" json:"stderr"`
	Status     string  `redis:"status" json:"status"`
	PrevStatus string  `redis:"prevstatus" json:"-"`
}

type AllStats struct {
	Stats  []Stats `redis:"stats" json:"stats"`
	Config config  `redis:"config" json:"config"`
}

type config struct {
	SMTP       SMTP                `json:"smtp"`
	Globals    Global              `json:"globals"`
	Commands   map[string]Command  `json:"commands"`
	Alarms     map[string]Alarm    `json:"alarms"`
	Checks     []*Check            `json:"checks"`
	Checklists map[string][]string `json:"checklists"`
}

var Config config
var db redis.Conn

var f_schedule = flag.String("schedule", "", "Name/ID of the schedule to run.")
var f_json = flag.Bool("json", false, "Output all checks statistics.")
var f_config = flag.String("config", "/etc/minimon.json", "Name of the config file to use.")
var f_verbose = flag.Bool("v", false, "Print debugging output.")
var f_daemon = flag.String("d", "", "Daemononize with this path to htdocs.")

func loadConfig(config string) (err error) {
	f, err := os.Open(config)
	if err != nil {
		return err
	}
	defer f.Close()

	b := new(bytes.Buffer)
	_, err = b.ReadFrom(f)
	if err != nil {
		return err
	}

	err = json.Unmarshal(b.Bytes(), &Config)
	if err != nil {
		return err
	}

	// Create md5 id's for all checks if not specified
	for _, c := range Config.Checks {
		if c.Id == "" {
			c.Id = md5sum(c.Command, strings.Join(c.Args, ""), c.Schedule, strings.Join(c.Alarms, ""))
		}
	}
	return nil
}

func initRedis() (err error) {
	db, err = redis.Dial("tcp", Config.Globals.Redis)
	if err != nil {
		return err
	}
	_, err = db.Do("SELECT", Config.Globals.RedisDB)
	return err
}

func Close() (err error) {
	return db.Close()
}

func runSchedule(sch string) {
	for _, c := range Config.Checks {
		if c.Schedule == sch {
			c.Run()
		}
	}
}

func getStats() (stats []Stats) {
	for _, c := range Config.Checks {
		if c.Checklist != "" {
			for _, cl := range Config.Checklists[c.Checklist] {
				stats = append(stats, c.Stats(cl))
			}
		} else {
			stats = append(stats, c.Stats(""))
		}
	}

	for _, c := range Config.Checks {
		c.Errors = 0
	}
	for _, s := range stats {
		if s.Status == "Error" {
			for _, c := range Config.Checks {
				if c.Id == s.CheckId {
					c.Errors++
				}
			}
		}
	}
	return
}

// --

func (c *Check) args(chlst string) (a []string) {
	curr := 0
	for _, arg := range strings.Split(Config.Commands[c.Command].Args, " ") {
		if strings.HasPrefix(arg, "$") { // command arg
			if c.Args[curr] == "$" { // checklist arg
				a = append(a, chlst)
			} else {
				// XXX thie can create slice index out of range if command wants more args than service supplies.
				a = append(a, c.Args[curr])
			}
			curr++
		} else {
			a = append(a, arg)
		}
	}
	return
}

func (c *Check) Run() {
	if c.Checklist != "" {
		for _, cl := range Config.Checklists[c.Checklist] {
			c.doCheck(cl)
		}
	} else {
		c.doCheck("")
	}
}

func (c *Check) getId(chlst string) (id, cmd string, args []string) {
	cmd = Config.Commands[c.Command].Command
	args = c.args(chlst)

	id = c.Command
	if chlst != "" {
		id += " " + chlst
	}
	return
}

func (c *Check) Stats(chlst string) (stats Stats) {
	id, cmd, args := c.getId(chlst)

	res, err := db.Do("HGETALL", id)
	panicif(err)
	vals, err := redis.Values(res, nil)
	panicif(err)
	err = redis.ScanStruct(vals, &stats)
	panicif(err)

	stats.Id = id
	stats.CheckId = c.Id
	stats.Command = cmd + " " + strings.Join(args, " ")
	return
}

func (c *Check) doCheck(chlst string) {
	id, cmd, args := c.getId(chlst)

	db.Do("HSET", id, "lastcheck", time.Now().Unix())

	stdout, stderr, rc := c.exec(id, cmd, args)
	db.Do("HSET", id, "stdout", stdout)
	db.Do("HSET", id, "stderr", stderr)

	prev, _ := redis.String(db.Do("HGET", id, "prevstatus"))

	if rc != 0 {
		db.Do("HSET", id, "status", "Error")

		if prev != "Error" {
			db.Do("HSET", id, "prevstatus", "Error")
			c.triggerAlarm(id, "ERROR", stdout+stderr)
		}
	} else {
		db.Do("HSET", id, "status", "OK")
		db.Do("HSET", id, "lastok", time.Now().Unix())

		if prev != "OK" {
			db.Do("HSET", id, "prevstatus", "OK")
			if c.Report == "ok" {
				c.triggerAlarm(id, "OK", stdout+stderr)
			}
		}
	}
}

func (c *Check) exec(id, command string, args []string) (stdout, stderr string, rc int) {
	verbose("Executing: %s %s\n", command, strings.Join(args, " "))
	var bstdout, bstderr bytes.Buffer
	cmd := exec.Command(command, args...)
	cmd.Stdout = &bstdout
	cmd.Stderr = &bstderr

	t0 := time.Now()
	err := cmd.Run()
	t1 := time.Now()
	db.Do("HSET", id, "checktime", t1.Sub(t0).Seconds())

	stdout = strings.TrimSpace(bstdout.String())
	stderr = strings.TrimSpace(bstderr.String())

	if err != nil {
		stderr = stderr + "\n" + err.Error()
		rc = -1
	} else {
		rc = 0
	}
	return
}

func (c *Check) triggerAlarm(id, status, msg string) {
	r := strings.NewReplacer("\n", " ")
	report := fmt.Sprintf("%s %s [%.100s]", status, id, r.Replace(strings.TrimSpace(msg)))

	for _, a := range c.Alarms {
		switch Config.Alarms[a].Type {
		case "stderr":
			fmt.Fprintf(os.Stderr, "%s\n", report)
		case "email":
			sendmail(Config.Alarms[a].Args, report)
		}

		verbose("Alarmed %s: %s\n", Config.Alarms[a].Type, report)
	}
}

// --

func sendmail(to, msg string) {
	c, err := smtp.Dial(Config.SMTP.Server + ":25")
	panicif(err)

	err = c.StartTLS(&tls.Config{ServerName: Config.SMTP.Server})
	panicif(err)

	err = c.Auth(smtp.PlainAuth("", Config.SMTP.User, Config.SMTP.Password, Config.SMTP.Server))
	panicif(err)

	err = c.Mail(Config.SMTP.Addr)
	panicif(err)

	err = c.Rcpt(to)
	panicif(err)

	w, err := c.Data()
	panicif(err)

	fmt.Fprintf(w, "From: System Monitoring <%s>\r\n", Config.SMTP.Addr)
	fmt.Fprintf(w, "Date: %s\r\n", time.Now().Format(time.RFC822))
	fmt.Fprintf(w, "Subject: System Event\r\n")
	fmt.Fprintf(w, "\r\n")
	fmt.Fprintf(w, "%s", msg)

	err = w.Close()
	panicif(err)

	err = c.Quit()
	panicif(err)
}

func md5sum(in ...string) (sum string) {
	h := md5.New()
	fmt.Fprint(h, strings.Join(in, " "))
	sum = fmt.Sprintf("%x", h.Sum(nil))
	return
}

func verbose(s string, args ...interface{}) {
	if !*f_verbose {
		return
	}
	fmt.Printf(s, args...)
}

func panicif(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	flag.Parse()
	err := loadConfig(*f_config)
	panicif(err)

	err = initRedis()
	panicif(err)

	if *f_schedule != "" {
		runSchedule(*f_schedule)

	} else if *f_daemon != "" {
		http.Handle("/", http.FileServer(http.Dir(*f_daemon)))
		http.ListenAndServe(":8000", nil)

	} else if *f_json {
		b, _ := json.MarshalIndent(&AllStats{Stats: getStats(), Config: Config}, " ", "\t")
		fmt.Printf("%s\n", b)

	} else {
		flag.Usage()
		os.Exit(1)
	}
}
