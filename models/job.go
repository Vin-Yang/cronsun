package models

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	client "github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/mvcc/mvccpb"

	"sunteng/commons/log"
	"sunteng/cronsun/conf"
)

const (
	DefaultJobGroup = "default"
)

// 需要执行的 cron cmd 命令
// 注册到 /cronsun/cmd/groupName/<id>
type Job struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Group   string     `json:"group"`
	Command string     `json:"cmd"`
	User    string     `json:"user"`
	Rules   []*JobRule `json:"rules"`
	Pause   bool       `json:"pause"` // 可手工控制的状态

	// 执行任务的结点，用于记录 job log
	runOn string
}

type JobRule struct {
	ID             string   `json:"id"`
	Timer          string   `json:"timer"`
	GroupIDs       []string `json:"gids"`
	NodeIDs        []string `json:"nids"`
	ExcludeNodeIDs []string `json:"exclude_nids"`
}

type Cmd struct {
	*Job
	*JobRule

	Schedule string
}

func (c *Cmd) GetID() string {
	return c.Job.ID + c.JobRule.ID
}

// 优先取结点里的值，更新 group 时可用 gid 判断是否对 job 进行处理
func (j *JobRule) included(nid string, gs map[string]*Group) bool {
	for i, count := 0, len(j.NodeIDs); i < count; i++ {
		if nid == j.NodeIDs[i] {
			return true
		}
	}

	for _, gid := range j.GroupIDs {
		if g, ok := gs[gid]; ok && g.Included(nid) {
			return true
		}
	}

	return false
}

func GetJob(group, id string) (job *Job, err error) {
	job, _, err = GetJobAndRev(group, id)
	return
}

func GetJobAndRev(group, id string) (job *Job, rev int64, err error) {
	resp, err := DefalutClient.Get(JobKey(group, id))
	if err != nil {
		return
	}

	if resp.Count == 0 {
		err = ErrNotFound
		return
	}

	rev = resp.Kvs[0].ModRevision
	err = json.Unmarshal(resp.Kvs[0].Value, &job)
	return
}

func DeleteJob(group, id string) (resp *client.DeleteResponse, err error) {
	return DefalutClient.Delete(JobKey(group, id))
}

func GetJobs() (jobs map[string]*Job, err error) {
	resp, err := DefalutClient.Get(conf.Config.Cmd, client.WithPrefix())
	if err != nil {
		return
	}

	count := len(resp.Kvs)
	jobs = make(map[string]*Job, count)
	if count == 0 {
		return
	}

	for _, j := range resp.Kvs {
		job := new(Job)
		if e := json.Unmarshal(j.Value, job); e != nil {
			log.Warnf("job[%s] umarshal err: %s", string(j.Key), e.Error())
			continue
		}
		jobs[job.ID] = job
	}
	return
}

func WatchJobs() client.WatchChan {
	return DefalutClient.Watch(conf.Config.Cmd, client.WithPrefix())
}

func GetJobFromKv(kv *mvccpb.KeyValue) (job *Job, err error) {
	job = new(Job)
	if err = json.Unmarshal(kv.Value, job); err != nil {
		err = fmt.Errorf("job[%s] umarshal err: %s", string(kv.Key), err.Error())
	}
	return
}

func (j *Job) GetID() string {
	return j.ID
}

func (j *Job) RunOn(n string) {
	j.runOn = n
}

func (j *Job) String() string {
	data, err := json.Marshal(j)
	if err != nil {
		return err.Error()
	}
	return string(data)
}

// Run 执行任务
func (j *Job) Run() {
	t, args := time.Now(), strings.Split(j.Command, " ")
	cmd := exec.Command(args[0], args[1:]...)

	if len(j.User) > 0 {
		u, err := user.Lookup(j.User)
		if err != nil {
			j.Fail(t, err.Error())
			return
		}

		uid, err := strconv.Atoi(u.Uid)
		if err != nil {
			if err != nil {
				j.Fail(t, "not support run with user on windows")
				return
			}
		}
		gid, _ := strconv.Atoi(u.Gid)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid: uint32(uid),
				Gid: uint32(gid),
			},
		}
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		j.Fail(t, fmt.Sprintf("%s\n\n%s", err.Error(), string(out)))
		return
	}

	j.Success(t, string(out))
}

// 从 etcd 的 key 中取 id
func GetIDFromKey(key string) string {
	index := strings.LastIndex(key, "/")
	if index < 0 {
		return ""
	}

	return key[index+1:]
}

func JobKey(group, id string) string {
	return conf.Config.Cmd + group + "/" + id
}

func (j *Job) Key() string {
	return JobKey(j.Group, j.ID)
}

func (j *Job) Check() error {
	j.ID = strings.TrimSpace(j.ID)
	if !IsValidAsKeyPath(j.ID) {
		return ErrIllegalJobId
	}

	j.Name = strings.TrimSpace(j.Name)
	if len(j.Name) == 0 {
		return ErrEmptyJobName
	}

	j.Group = strings.TrimSpace(j.Group)
	if len(j.Group) == 0 {
		j.Group = DefaultJobGroup
	}

	if !IsValidAsKeyPath(j.Group) {
		return ErrIllegalJobGroupName
	}

	j.User = strings.TrimSpace(j.User)

	for i := range j.Rules {
		id := strings.TrimSpace(j.Rules[i].ID)
		if id == "" || strings.HasPrefix(id, "NEW") {
			j.Rules[i].ID = NextID()
		}
	}

	// 不修改 Command 的内容，简单判断是否为空
	if len(strings.TrimSpace(j.Command)) == 0 {
		return ErrEmptyJobCommand
	}

	return nil
}

// 执行结果写入 mongoDB
func (j *Job) Success(t time.Time, out string) {
	CreateJobLog(j, t, out, true)
}

func (j *Job) Fail(t time.Time, msg string) {
	CreateJobLog(j, t, msg, false)
}

func (j *Job) Cmds(nid string, gs map[string]*Group) (cmds map[string]*Cmd) {
	cmds = make(map[string]*Cmd)
	if j.Pause {
		return
	}

	for _, r := range j.Rules {
		for _, id := range r.ExcludeNodeIDs {
			if nid == id {
				continue
			}
		}

		if r.included(nid, gs) {
			cmd := &Cmd{
				Job:     j,
				JobRule: r,

				Schedule: r.Timer,
			}
			j.RunOn(nid)
			cmds[cmd.GetID()] = cmd
		}
	}

	return
}
