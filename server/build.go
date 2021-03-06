package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/cncd/pipeline/pipeline/rpc"
	"github.com/cncd/pubsub"
	"github.com/cncd/queue"
	"github.com/drone/drone/remote"
	"github.com/drone/drone/shared/httputil"
	"github.com/drone/drone/store"
	"github.com/gin-gonic/gin"

	"github.com/drone/drone/model"
	"github.com/drone/drone/router/middleware/session"
)

func GetBuilds(c *gin.Context) {
	repo := session.Repo(c)
	builds, err := store.GetBuildList(c, repo)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.JSON(http.StatusOK, builds)
}

func GetBuild(c *gin.Context) {
	if c.Param("number") == "latest" {
		GetBuildLast(c)
		return
	}

	repo := session.Repo(c)
	num, err := strconv.Atoi(c.Param("number"))
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	build, err := store.GetBuildNumber(c, repo, num)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	procs, _ := store.FromContext(c).ProcList(build)
	build.Procs = model.Tree(procs)

	c.JSON(http.StatusOK, build)
}

func GetBuildLast(c *gin.Context) {
	repo := session.Repo(c)
	branch := c.DefaultQuery("branch", repo.Branch)

	build, err := store.GetBuildLast(c, repo, branch)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}

	procs, _ := store.FromContext(c).ProcList(build)
	build.Procs = model.Tree(procs)
	c.JSON(http.StatusOK, build)
}

func GetBuildLogs(c *gin.Context) {
	repo := session.Repo(c)

	// parse the build number and job sequence number from
	// the repquest parameter.
	num, _ := strconv.Atoi(c.Params.ByName("number"))
	ppid, _ := strconv.Atoi(c.Params.ByName("ppid"))
	name := c.Params.ByName("proc")

	build, err := store.GetBuildNumber(c, repo, num)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}

	proc, err := store.FromContext(c).ProcChild(build, ppid, name)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}

	rc, err := store.FromContext(c).LogFind(proc)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}

	defer rc.Close()

	c.Header("Content-Type", "application/json")
	io.Copy(c.Writer, rc)
}

func DeleteBuild(c *gin.Context) {
	repo := session.Repo(c)

	// parse the build number and job sequence number from
	// the repquest parameter.
	num, _ := strconv.Atoi(c.Params.ByName("number"))
	seq, _ := strconv.Atoi(c.Params.ByName("job"))

	build, err := store.GetBuildNumber(c, repo, num)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}

	proc, err := store.FromContext(c).ProcFind(build, seq)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}

	if proc.State != model.StatusRunning {
		c.String(400, "Cannot cancel a non-running build")
		return
	}

	proc.State = model.StatusKilled
	proc.Stopped = time.Now().Unix()
	if proc.Started == 0 {
		proc.Started = proc.Stopped
	}
	proc.ExitCode = 137
	// TODO cancel child procs
	store.FromContext(c).ProcUpdate(proc)

	config.queue.Error(context.Background(), fmt.Sprint(proc.ID), queue.ErrCancel)
	c.String(204, "")
}

func PostApproval(c *gin.Context) {
	var (
		remote_ = remote.FromContext(c)
		repo    = session.Repo(c)
		user    = session.User(c)
		num, _  = strconv.Atoi(
			c.Params.ByName("number"),
		)
	)

	build, err := store.GetBuildNumber(c, repo, num)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}
	if build.Status != model.StatusBlocked {
		c.String(500, "cannot decline a build with status %s", build.Status)
		return
	}
	build.Status = model.StatusPending
	build.Reviewed = time.Now().Unix()
	build.Reviewer = user.Login

	//
	//
	// This code is copied pasted until I have a chance
	// to refactor into a proper function. Lots of changes
	// and technical debt. No judgement please!
	//
	//

	// fetch the build file from the database
	raw, err := remote_.File(user, repo, build, repo.Config)
	if err != nil {
		logrus.Errorf("failure to get build config for %s. %s", repo.FullName, err)
		c.AbortWithError(404, err)
		return
	}

	netrc, err := remote_.Netrc(user, repo)
	if err != nil {
		c.String(500, "Failed to generate netrc file. %s", err)
		return
	}

	if uerr := store.UpdateBuild(c, build); err != nil {
		c.String(500, "error updating build. %s", uerr)
		return
	}

	c.JSON(200, build)

	// get the previous build so that we can send
	// on status change notifications
	last, _ := store.GetBuildLastBefore(c, repo, build.Branch, build.ID)
	secs, err := store.GetMergedSecretList(c, repo)
	if err != nil {
		logrus.Debugf("Error getting secrets for %s#%d. %s", repo.FullName, build.Number, err)
	}

	defer func() {
		uri := fmt.Sprintf("%s/%s/%d", httputil.GetURL(c.Request), repo.FullName, build.Number)
		err = remote_.Status(user, repo, build, uri)
		if err != nil {
			logrus.Errorf("error setting commit status for %s/%d", repo.FullName, build.Number)
		}
	}()

	b := builder{
		Repo:  repo,
		Curr:  build,
		Last:  last,
		Netrc: netrc,
		Secs:  secs,
		Link:  httputil.GetURL(c.Request),
		Yaml:  string(raw),
	}
	items, err := b.Build()
	if err != nil {
		build.Status = model.StatusError
		build.Started = time.Now().Unix()
		build.Finished = build.Started
		build.Error = err.Error()
		store.UpdateBuild(c, build)
		return
	}

	var pcounter = len(items)
	for _, item := range items {
		build.Procs = append(build.Procs, item.Proc)
		item.Proc.BuildID = build.ID

		for _, stage := range item.Config.Stages {
			var gid int
			for _, step := range stage.Steps {
				pcounter++
				if gid == 0 {
					gid = pcounter
				}
				proc := &model.Proc{
					BuildID: build.ID,
					Name:    step.Alias,
					PID:     pcounter,
					PPID:    item.Proc.PID,
					PGID:    gid,
					State:   model.StatusPending,
				}
				build.Procs = append(build.Procs, proc)
			}
		}
	}
	store.FromContext(c).ProcCreate(build.Procs)

	//
	// publish topic
	//
	buildCopy := *build
	buildCopy.Procs = model.Tree(buildCopy.Procs)
	message := pubsub.Message{
		Labels: map[string]string{
			"repo":    repo.FullName,
			"private": strconv.FormatBool(repo.IsPrivate),
		},
	}
	message.Data, _ = json.Marshal(model.Event{
		Type:  model.Enqueued,
		Repo:  *repo,
		Build: buildCopy,
	})
	// TODO remove global reference
	config.pubsub.Publish(c, "topic/events", message)

	//
	// end publish topic
	//

	for _, item := range items {
		task := new(queue.Task)
		task.ID = fmt.Sprint(item.Proc.ID)
		task.Labels = map[string]string{}
		task.Labels["platform"] = item.Platform
		for k, v := range item.Labels {
			task.Labels[k] = v
		}

		task.Data, _ = json.Marshal(rpc.Pipeline{
			ID:      fmt.Sprint(item.Proc.ID),
			Config:  item.Config,
			Timeout: b.Repo.Timeout,
		})

		config.logger.Open(context.Background(), task.ID)
		config.queue.Push(context.Background(), task)
	}
}

func PostDecline(c *gin.Context) {
	var (
		remote_ = remote.FromContext(c)
		repo    = session.Repo(c)
		user    = session.User(c)
		num, _  = strconv.Atoi(
			c.Params.ByName("number"),
		)
	)

	build, err := store.GetBuildNumber(c, repo, num)
	if err != nil {
		c.AbortWithError(404, err)
		return
	}
	if build.Status != model.StatusBlocked {
		c.String(500, "cannot decline a build with status %s", build.Status)
		return
	}
	build.Status = model.StatusDeclined
	build.Reviewed = time.Now().Unix()
	build.Reviewer = user.Login

	err = store.UpdateBuild(c, build)
	if err != nil {
		c.String(500, "error updating build. %s", err)
		return
	}

	uri := fmt.Sprintf("%s/%s/%d", httputil.GetURL(c.Request), repo.FullName, build.Number)
	err = remote_.Status(user, repo, build, uri)
	if err != nil {
		logrus.Errorf("error setting commit status for %s/%d", repo.FullName, build.Number)
	}

	c.JSON(200, build)
}

func GetBuildQueue(c *gin.Context) {
	out, err := store.GetBuildQueue(c)
	if err != nil {
		c.String(500, "Error getting build queue. %s", err)
		return
	}
	c.JSON(200, out)
}

//
//
//
//
//
//

func PostBuild(c *gin.Context) {

	remote_ := remote.FromContext(c)
	repo := session.Repo(c)
	fork := c.DefaultQuery("fork", "false")

	num, err := strconv.Atoi(c.Param("number"))
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	user, err := store.GetUser(c, repo.UserID)
	if err != nil {
		logrus.Errorf("failure to find repo owner %s. %s", repo.FullName, err)
		c.AbortWithError(500, err)
		return
	}

	build, err := store.GetBuildNumber(c, repo, num)
	if err != nil {
		logrus.Errorf("failure to get build %d. %s", num, err)
		c.AbortWithError(404, err)
		return
	}

	// if the remote has a refresh token, the current access token
	// may be stale. Therefore, we should refresh prior to dispatching
	// the job.
	if refresher, ok := remote_.(remote.Refresher); ok {
		ok, _ := refresher.Refresh(user)
		if ok {
			store.UpdateUser(c, user)
		}
	}

	// fetch the .drone.yml file from the database
	raw, err := remote_.File(user, repo, build, repo.Config)
	if err != nil {
		logrus.Errorf("failure to get build config for %s. %s", repo.FullName, err)
		c.AbortWithError(404, err)
		return
	}

	netrc, err := remote_.Netrc(user, repo)
	if err != nil {
		logrus.Errorf("failure to generate netrc for %s. %s", repo.FullName, err)
		c.AbortWithError(500, err)
		return
	}

	// must not restart a running build
	if build.Status == model.StatusPending || build.Status == model.StatusRunning {
		c.String(409, "Cannot re-start a started build")
		return
	}

	// forking the build creates a duplicate of the build
	// and then executes. This retains prior build history.
	if forkit, _ := strconv.ParseBool(fork); forkit {
		build.ID = 0
		build.Number = 0
		build.Parent = num
		build.Status = model.StatusPending
		build.Started = 0
		build.Finished = 0
		build.Enqueued = time.Now().UTC().Unix()
		build.Error = ""
		err = store.CreateBuild(c, build)
		if err != nil {
			c.String(500, err.Error())
			return
		}

		event := c.DefaultQuery("event", build.Event)
		if event == model.EventPush ||
			event == model.EventPull ||
			event == model.EventTag ||
			event == model.EventDeploy {
			build.Event = event
		}
		build.Deploy = c.DefaultQuery("deploy_to", build.Deploy)
	} else {
		// todo move this to database tier
		// and wrap inside a transaction
		build.Status = model.StatusPending
		build.Started = 0
		build.Finished = 0
		build.Enqueued = time.Now().UTC().Unix()
		build.Error = ""

		err = store.FromContext(c).ProcClear(build)
		if err != nil {
			c.AbortWithStatus(500)
			return
		}

		err = store.UpdateBuild(c, build)
		if err != nil {
			c.AbortWithStatus(500)
			return
		}
	}

	// Read query string parameters into buildParams, exclude reserved params
	var buildParams = map[string]string{}
	for key, val := range c.Request.URL.Query() {
		switch key {
		case "fork", "event", "deploy_to":
		default:
			// We only accept string literals, because build parameters will be
			// injected as environment variables
			buildParams[key] = val[0]
		}
	}

	// get the previous build so that we can send
	// on status change notifications
	last, _ := store.GetBuildLastBefore(c, repo, build.Branch, build.ID)
	secs, err := store.GetMergedSecretList(c, repo)
	if err != nil {
		logrus.Debugf("Error getting secrets for %s#%d. %s", repo.FullName, build.Number, err)
	}

	b := builder{
		Repo:  repo,
		Curr:  build,
		Last:  last,
		Netrc: netrc,
		Secs:  secs,
		Link:  httputil.GetURL(c.Request),
		Yaml:  string(raw),
	}
	// TODO inject environment varibles !!!!!! buildParams
	items, err := b.Build()
	if err != nil {
		build.Status = model.StatusError
		build.Started = time.Now().Unix()
		build.Finished = build.Started
		build.Error = err.Error()
		c.JSON(500, build)
		return
	}

	var pcounter = len(items)
	for _, item := range items {
		build.Procs = append(build.Procs, item.Proc)
		item.Proc.BuildID = build.ID

		for _, stage := range item.Config.Stages {
			var gid int
			for _, step := range stage.Steps {
				pcounter++
				if gid == 0 {
					gid = pcounter
				}
				proc := &model.Proc{
					BuildID: build.ID,
					Name:    step.Alias,
					PID:     pcounter,
					PPID:    item.Proc.PID,
					PGID:    gid,
					State:   model.StatusPending,
				}
				build.Procs = append(build.Procs, proc)
			}
		}
	}

	err = store.FromContext(c).ProcCreate(build.Procs)
	if err != nil {
		logrus.Errorf("cannot restart %s#%d: %s", repo.FullName, build.Number, err)
		build.Status = model.StatusError
		build.Started = time.Now().Unix()
		build.Finished = build.Started
		build.Error = err.Error()
		c.JSON(500, build)
		return
	}

	c.JSON(202, build)

	//
	// publish topic
	//
	buildCopy := *build
	buildCopy.Procs = model.Tree(buildCopy.Procs)
	message := pubsub.Message{
		Labels: map[string]string{
			"repo":    repo.FullName,
			"private": strconv.FormatBool(repo.IsPrivate),
		},
	}
	message.Data, _ = json.Marshal(model.Event{
		Type:  model.Enqueued,
		Repo:  *repo,
		Build: buildCopy,
	})
	// TODO remove global reference
	config.pubsub.Publish(c, "topic/events", message)
	//
	// end publish topic
	//

	for _, item := range items {
		task := new(queue.Task)
		task.ID = fmt.Sprint(item.Proc.ID)
		task.Labels = map[string]string{}
		task.Labels["platform"] = item.Platform
		for k, v := range item.Labels {
			task.Labels[k] = v
		}

		task.Data, _ = json.Marshal(rpc.Pipeline{
			ID:      fmt.Sprint(item.Proc.ID),
			Config:  item.Config,
			Timeout: b.Repo.Timeout,
		})

		config.logger.Open(context.Background(), task.ID)
		config.queue.Push(context.Background(), task)
	}
}
