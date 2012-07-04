package app

import (
	"errors"
	"fmt"
	"github.com/timeredbull/tsuru/api/auth"
	"github.com/timeredbull/tsuru/api/unit"
	"github.com/timeredbull/tsuru/config"
	"github.com/timeredbull/tsuru/db"
	"github.com/timeredbull/tsuru/fs"
	"github.com/timeredbull/tsuru/log"
	"github.com/timeredbull/tsuru/repository"
	"labix.org/v2/mgo/bson"
	"launchpad.net/goyaml"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

const confSep = "========"

type EnvVar struct {
	Name        string
	Value       string
	Public      bool
	ServiceName string
}

func (e *EnvVar) String() string {
	var value, suffix string
	if e.Public {
		value = e.Value
	} else {
		value = "***"
		suffix = " (private variable)"
	}
	return fmt.Sprintf("%s=%s%s", e.Name, value, suffix)
}

type App struct {
	Env       map[string]EnvVar
	Framework string
	Name      string
	State     string
	Units     []unit.Unit
	Teams     []string
	Logs      []Log
	fsystem   fs.Fs
}

type Log struct {
	Date    time.Time
	Message string
}

type conf struct {
	PreRestart string `yaml:"pre-restart"`
	PosRestart string `yaml:"pos-restart"`
}

func AllApps() ([]App, error) {
	var apps []App
	err := db.Session.Apps().Find(nil).All(&apps)
	return apps, err
}

func (a *App) Get() error {
	return db.Session.Apps().Find(bson.M{"name": a.Name}).One(&a)
}

func (a *App) Create() error {
	a.State = "PENDING"
	err := db.Session.Apps().Insert(a)
	if err != nil {
		return err
	}
	a.Log(fmt.Sprintf("creating app %s", a.Name))
	cmd := exec.Command("juju", "deploy", "--repository=/home/charms", "local:"+a.Framework, a.Name)
	log.Printf("deploying %s with name %s", a.Framework, a.Name)
	out, err := cmd.CombinedOutput()
	a.Log(string(out))
	if err != nil {
		return err
	}
	a.Log(fmt.Sprintf("app %s successfully created", a.Name))
	return nil
}

func (a *App) Destroy() error {
	err := db.Session.Apps().Remove(bson.M{"name": a.Name})
	if err != nil {
		return err
	}
	go func(a *App) {
		p, _ := repository.GetBarePath(a.Name)
		a.fs().RemoveAll(p)
	}(a)
	u := a.unit()
	cmd := exec.Command("juju", "destroy-service", a.Name)
	log.Printf("destroying %s with name %s", a.Framework, a.Name)
	out, err := cmd.CombinedOutput()
	log.Printf(string(out))
	if err != nil {
		return err
	}
	cmd = exec.Command("juju", "terminate-machine", strconv.Itoa(u.Machine))
	log.Printf("terminating machine %d", u.Machine)
	out, err = cmd.CombinedOutput()
	log.Printf(string(out))
	if err != nil {
		return err
	}
	return nil
}

func (a *App) AddOrUpdateUnit(u *unit.Unit) {
	for i, unt := range a.Units {
		if unt.InstanceId == u.InstanceId {
			a.Units[i].Ip = u.Ip
			a.Units[i].AgentState = u.AgentState
			a.Units[i].InstanceState = u.InstanceState
			return
		}
	}
	a.Units = append(a.Units, *u)
}

func (a *App) fs() fs.Fs {
	if a.fsystem == nil {
		a.fsystem = fs.OsFs{}
	}
	return a.fsystem
}

func (a *App) findTeam(team *auth.Team) int {
	for i, t := range a.Teams {
		if t == team.Name {
			return i
		}
	}
	return -1
}

func (a *App) hasTeam(team *auth.Team) bool {
	return a.findTeam(team) > -1
}

func (a *App) GrantAccess(team *auth.Team) error {
	if a.hasTeam(team) {
		return errors.New("This team has already access to this app")
	}
	a.Teams = append(a.Teams, team.Name)
	return nil
}

func (a *App) RevokeAccess(team *auth.Team) error {
	index := a.findTeam(team)
	if index < 0 {
		return errors.New("This team does not have access to this app")
	}
	last := len(a.Teams) - 1
	a.Teams[index] = a.Teams[last]
	a.Teams = a.Teams[:last]
	return nil
}

func (a *App) GetTeams() (teams []auth.Team) {
	db.Session.Teams().Find(bson.M{"name": bson.M{"$in": a.Teams}}).All(&teams)
	return
}

func (a *App) setTeams(teams []auth.Team) {
	a.Teams = make([]string, len(teams))
	for i, team := range teams {
		a.Teams[i] = team.Name
	}
}

func (a *App) CheckUserAccess(user *auth.User) bool {
	for _, team := range a.GetTeams() {
		if team.ContainsUser(user) {
			return true
		}
	}
	return false
}

func (a *App) SetEnv(name, value string, public bool) {
	if a.Env == nil {
		a.Env = make(map[string]EnvVar)
	}
	env := EnvVar{
		Name:   name,
		Value:  value,
		Public: public,
	}
	a.Env[name] = env
	a.Log(fmt.Sprintf("setting env %s with value %s", name, value))
}

func (a *App) GetEnv(name string) (env EnvVar, err error) {
	var ok bool
	if env, ok = a.Env[name]; !ok {
		err = errors.New("Environment variable not declared for this app.")
	}
	return
}

func deployHookAbsPath(p string) (string, error) {
	repoPath, err := config.GetString("git:unit-repo")
	if err != nil {
		return "", nil
	}
	return path.Join(repoPath, p), nil
}

/*
* Returns app.conf located at app's git repository
 */
func (a *App) conf() (conf, error) {
	var c conf
	u := a.unit()
	uRepo, err := repository.GetPath()
	if err != nil {
		a.Log(fmt.Sprintf("Got error while getting repository path: %s", err.Error()))
		return c, err
	}
	cPath := path.Join(uRepo, "app.conf")
	cmd := fmt.Sprintf(`echo "%s";cat %s`, confSep, cPath)
	o, err := u.Command(cmd)
	if err != nil {
		a.Log(fmt.Sprintf("Got error while executing command: %s... Skipping hooks execution", err.Error()))
		return c, nil
	}
	data := strings.Split(string(o), confSep)[1]
	err = goyaml.Unmarshal([]byte(data), &c)
	if err != nil {
		a.Log(fmt.Sprintf("Got error while parsing yaml: %s", err.Error()))
		return c, err
	}
	return c, nil
}

/*
* preRestart is responsible for running user's pre-restart script.
* The path to this script can be found at the app.conf file, at the root of user's app repository.
 */
func (a *App) preRestart(c conf) error {
	if !a.hasRestartHooks(c) {
		a.Log("app.conf file does not exists or is in the right place. Skipping...")
		return nil
	}
	if c.PreRestart == "" {
		a.Log("pre-restart hook section in app conf does not exists... Skipping...")
		return nil
	}
	u := a.unit()
	p, err := deployHookAbsPath(c.PreRestart)
	if err != nil {
		a.Log(fmt.Sprintf("Error obtaining absolute path to hook: %s. Skipping...", err))
		return nil
	}
	out, err := u.Command("/bin/bash", p)
	a.Log("Executing pre-restart hook...")
	a.Log(fmt.Sprintf("Output of pre-restart hook: %s", string(out)))
	return err
}

/*
* posRestart is responsible for running user's pos-restart script.
* The path to this script can be found at the app.conf file, at the root of user's app repository.
 */
func (a *App) posRestart(c conf) error {
	if !a.hasRestartHooks(c) {
		a.Log("app.conf file does not exists or is in the right place. Skipping...")
		return nil
	}
	if c.PosRestart == "" {
		a.Log("pos-restart hook section in app conf does not exists... Skipping...")
		return nil
	}
	u := a.unit()
	p, err := deployHookAbsPath(c.PosRestart)
	if err != nil {
		a.Log(fmt.Sprintf("Error obtaining absolute path to hook: %s. Skipping...", err))
		return nil
	}
	out, err := u.Command("/bin/bash", p)
	a.Log("Executing pos-restart hook...")
	a.Log(fmt.Sprintf("Output of pos-restart hook: %s", string(out)))
	return err
}

func (a *App) hasRestartHooks(c conf) bool {
	return !(c.PreRestart == "" && c.PosRestart == "")
}

func (a *App) updateHooks() error {
	u := a.unit()
	a.Log("executting hook dependencies")
	out, err := u.ExecuteHook("dependencies")
	a.Log(string(out))
	if err != nil {
		return err
	}
	a.Log("executting hook reload-gunicorn")
	out, err = u.ExecuteHook("reload-gunicorn")
	a.Log(string(out))
	if err != nil {
		return err
	}
	return nil
}

func (a *App) unit() unit.Unit {
	if len(a.Units) > 0 {
		return a.Units[0]
	}
	return unit.Unit{}
}

func (a *App) Log(message string) error {
	log.Printf(message)
	l := Log{Date: time.Now(), Message: message}
	a.Logs = append(a.Logs, l)
	return db.Session.Apps().Update(bson.M{"name": a.Name}, a)
}

// GetApps returns all apps that the given team has access to
func GetApps(team *auth.Team) (apps []App, err error) {
	if team == nil {
		err = errors.New("You must provide the team.")
		return
	}
	err = db.Session.Apps().Find(bson.M{"teams": team.Name}).All(&apps)
	return
}
