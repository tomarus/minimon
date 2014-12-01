package main

import (
	"testing"
)

func init() {
	e := loadConfig("minimon_test.json")
	if e != nil {
		panic(e)
	}

	e = initRedis()
	if e != nil {
		panic(e)
	}
}

func TestDumpconfig(t *testing.T) {
	t.Logf("%#v", Config)
}

func TestService1(t *testing.T) {
	runSchedule("test")
}
