package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"strconv"
	"strings"
	"time"

	"github.com/evilsocket/opensnitch/daemon/core"
	"github.com/evilsocket/opensnitch/daemon/firewall"
	"github.com/evilsocket/opensnitch/daemon/log"
	"github.com/evilsocket/opensnitch/daemon/procmon/monitor"
	"github.com/evilsocket/opensnitch/daemon/rule"
	"github.com/evilsocket/opensnitch/daemon/tasks"
	"github.com/evilsocket/opensnitch/daemon/tasks/nodemonitor"
	"github.com/evilsocket/opensnitch/daemon/tasks/pidmonitor"
	"github.com/evilsocket/opensnitch/daemon/tasks/socketsmonitor"
	"github.com/evilsocket/opensnitch/daemon/ui/config"
	"github.com/evilsocket/opensnitch/daemon/ui/protocol"
	"golang.org/x/net/context"
)

// NewReply constructs a new protocol notification reply
func NewReply(rID uint64, replyCode protocol.NotificationReplyCode, data string) *protocol.NotificationReply {
	return &protocol.NotificationReply{
		Id:   rID,
		Code: replyCode,
		Data: data,
	}
}

func (c *Client) getClientConfig() *protocol.ClientConfig {
	raw, _ := ioutil.ReadFile(configFile)
	nodeName := core.GetHostname()
	nodeVersion := core.GetKernelVersion()
	var ts time.Time
	rulesTotal := len(c.rules.GetAll())
	ruleList := make([]*protocol.Rule, rulesTotal)
	idx := 0
	for _, r := range c.rules.GetAll() {
		ruleList[idx] = r.Serialize()
		idx++
	}
	sysfw, err := firewall.Serialize()
	if err != nil {
		log.Warning("firewall.Serialize() error: %s", err)
	}
	return &protocol.ClientConfig{
		Id:                uint64(ts.UnixNano()),
		Name:              nodeName,
		Version:           nodeVersion,
		IsFirewallRunning: firewall.IsRunning(),
		Config:            strings.Replace(string(raw), "\n", "", -1),
		LogLevel:          uint32(log.MinLevel),
		Rules:             ruleList,
		SystemFirewall:    sysfw,
	}
}

func (c *Client) handleActionChangeConfig(stream protocol.UI_NotificationsClient, notification *protocol.Notification) {
	log.Info("[notification] Reloading configuration")
	// Parse received configuration first, to get the new proc monitor method.
	newConf, err := config.Parse(notification.Data)
	if err != nil {
		log.Warning("[notification] error parsing received config: %v", notification.Data)
		c.sendNotificationReply(stream, notification.Id, "", err)
		return
	}

	if err := c.reloadConfiguration(true, &newConf); err != nil {
		c.sendNotificationReply(stream, notification.Id, "", err.Msg)
		return
	}

	// this save operation triggers a regular re-loadConfiguration()
	err = config.Save(configFile, notification.Data)
	if err != nil {
		log.Warning("[notification] CHANGE_CONFIG not applied %s", err)
	}

	c.sendNotificationReply(stream, notification.Id, "", err)
}

func (c *Client) handleActionEnableRule(stream protocol.UI_NotificationsClient, notification *protocol.Notification) {
	var err error
	for _, rul := range notification.Rules {
		log.Info("[notification] enable rule: %s", rul.Name)
		// protocol.Rule(protobuf) != rule.Rule(json)
		r, _ := rule.Deserialize(rul)
		r.Enabled = true
		// save to disk only if the duration is rule.Always
		err = c.rules.Replace(r, r.Duration == rule.Always)
	}
	c.sendNotificationReply(stream, notification.Id, "", err)
}

func (c *Client) handleActionDisableRule(stream protocol.UI_NotificationsClient, notification *protocol.Notification) {
	var err error
	for _, rul := range notification.Rules {
		log.Info("[notification] disable rule: %s", rul)
		r, _ := rule.Deserialize(rul)
		r.Enabled = false
		err = c.rules.Replace(r, r.Duration == rule.Always)
	}
	c.sendNotificationReply(stream, notification.Id, "", err)
}

func (c *Client) handleActionChangeRule(stream protocol.UI_NotificationsClient, notification *protocol.Notification) {
	var rErr error
	for _, rul := range notification.Rules {
		r, err := rule.Deserialize(rul)
		if r == nil {
			rErr = fmt.Errorf("Invalid rule, %s", err)
			continue
		}
		log.Info("[notification] change rule: %s %d", r, notification.Id)
		if err := c.rules.Replace(r, r.Duration == rule.Always); err != nil {
			log.Warning("[notification] Error changing rule: %s %s", err, r)
			rErr = err
		}
	}
	c.sendNotificationReply(stream, notification.Id, "", rErr)
}

func (c *Client) handleActionDeleteRule(stream protocol.UI_NotificationsClient, notification *protocol.Notification) {
	var err error
	for _, rul := range notification.Rules {
		log.Info("[notification] delete rule: %s %d", rul.Name, notification.Id)
		err = c.rules.Delete(rul.Name)
		if err != nil {
			log.Error("[notification] Error deleting rule: %s %s", err, rul)
		}
	}
	c.sendNotificationReply(stream, notification.Id, "", err)
}

func (c *Client) handleActionTaskStart(stream protocol.UI_NotificationsClient, notification *protocol.Notification) {
	var taskConf tasks.TaskNotification
	err := json.Unmarshal([]byte(notification.Data), &taskConf)
	if err != nil {
		log.Error("parsing TaskStart, err: %s, %s", err, notification.Data)
		c.sendNotificationReply(stream, notification.Id, "", err)
		return
	}
	switch taskConf.Name {
	case pidmonitor.Name:
		conf, ok := taskConf.Data.(map[string]interface{})
		if !ok {
			log.Error("[pidmon] TaskStart.Data, PID err (string expected): %v", taskConf)
			return
		}
		pid, err := strconv.Atoi(conf["pid"].(string))
		if err != nil {
			log.Error("[pidmon] TaskStart.Data, PID err: %s, %v", err, taskConf)
			c.sendNotificationReply(stream, notification.Id, "", err)
			return
		}
		interval, _ := conf["interval"].(string)
		c.monitorProcessDetails(pid, interval, stream, notification)
	case nodemonitor.Name:
		conf, ok := taskConf.Data.(map[string]interface{})
		if !ok {
			log.Error("[nodemon] TaskStart.Data, \"node\" err (string expected): %v", taskConf)
			return
		}
		c.monitorNode(conf["node"].(string), conf["interval"].(string), stream, notification)
	case socketsmonitor.Name:
		c.monitorSockets(taskConf.Data, stream, notification)
	default:
		log.Debug("TaskStart, unknown task: %v", taskConf)
		//c.sendNotificationReply(stream, notification.Id, "", err)
	}
}

func (c *Client) handleActionTaskStop(stream protocol.UI_NotificationsClient, notification *protocol.Notification) {
	var taskConf tasks.TaskNotification
	err := json.Unmarshal([]byte(notification.Data), &taskConf)
	if err != nil {
		log.Error("parsing TaskStop, err: %s, %s", err, notification.Data)
		c.sendNotificationReply(stream, notification.Id, "", fmt.Errorf("Error stopping task: %s", notification.Data))
		return
	}
	switch taskConf.Name {
	case pidmonitor.Name:
		conf, ok := taskConf.Data.(map[string]interface{})
		if !ok {
			log.Error("[pidmon] TaskStop.Data, PID err (string expected): %v", taskConf)
			return
		}
		pid, err := strconv.Atoi(conf["pid"].(string))
		if err != nil {
			log.Error("TaskStop.Data, err: %s, %s, %v+, %q", err, notification.Data, taskConf.Data, taskConf.Data)
			c.sendNotificationReply(stream, notification.Id, "", err)
			return
		}
		TaskMgr.RemoveTask(fmt.Sprint(taskConf.Name, "-", pid))
	case nodemonitor.Name:
		conf, ok := taskConf.Data.(map[string]interface{})
		if !ok {
			log.Error("[pidmon] TaskStop.Data, PID err (string expected): %v", taskConf)
			return
		}
		TaskMgr.RemoveTask(fmt.Sprint(nodemonitor.Name, "-", conf["node"].(string)))
	case socketsmonitor.Name:
		TaskMgr.RemoveTask(socketsmonitor.Name)
	default:
		log.Debug("TaskStop, unknown task: %v", taskConf)
		//c.sendNotificationReply(stream, notification.Id, "", err)
	}
}

func (c *Client) handleActionEnableInterception(stream protocol.UI_NotificationsClient, notification *protocol.Notification) {
	log.Info("[notification] starting interception")
	if err := monitor.ReconfigureMonitorMethod(c.config.ProcMonitorMethod, c.config.Ebpf, c.config.Audit); err != nil && err.What > monitor.NoError {
		log.Warning("[notification] error enabling monitor (%s): %s", c.config.ProcMonitorMethod, err.Msg)
		c.sendNotificationReply(stream, notification.Id, "", err.Msg)
		return
	}
	if err := firewall.EnableInterception(); err != nil {
		log.Warning("[notification] firewall.EnableInterception() error: %s", err)
		c.sendNotificationReply(stream, notification.Id, "", err)
		return
	}
	c.sendNotificationReply(stream, notification.Id, "", nil)
}

func (c *Client) handleActionDisableInterception(stream protocol.UI_NotificationsClient, notification *protocol.Notification) {
	log.Info("[notification] stopping interception")
	monitor.End()
	if err := firewall.DisableInterception(); err != nil {
		log.Warning("firewall.DisableInterception() error: %s", err)
		c.sendNotificationReply(stream, notification.Id, "", err)
		return
	}
	c.sendNotificationReply(stream, notification.Id, "", nil)
}

func (c *Client) handleActionReloadFw(stream protocol.UI_NotificationsClient, notification *protocol.Notification) {
	log.Info("[notification] reloading firewall")

	sysfw, err := firewall.Deserialize(notification.SysFirewall)
	if err != nil {
		log.Warning("firewall.Deserialize() error: %s", err)
		c.sendNotificationReply(stream, notification.Id, "", fmt.Errorf("Error reloading firewall, invalid rules"))
		return
	}
	if err := firewall.SaveConfiguration(sysfw); err != nil {
		c.sendNotificationReply(stream, notification.Id, "", fmt.Errorf("Error saving system firewall rules: %s", err))
		return
	}
	// TODO:
	// - add new API endpoints to delete, add or change rules atomically.
	// - a global goroutine where errors can be sent to the server (GUI).
	go func(c *Client) {
		var errors string
		for {
			select {
			case fwerr := <-firewall.ErrorsChan():
				errors = fmt.Sprint(errors, fwerr, ",")
				if firewall.ErrChanEmpty() {
					goto ExitWithError
				}

			// FIXME: can this operation last longer than 2s? if there're more than.. 100...10000 rules?
			case <-time.After(2 * time.Second):
				log.Debug("[notification] reload firewall. timeout fired, no errors?")
				c.sendNotificationReply(stream, notification.Id, "", nil)
				goto Exit

			}
		}
	ExitWithError:
		c.sendNotificationReply(stream, notification.Id, "", fmt.Errorf("%s", errors))
	Exit:
	}(c)

}

func (c *Client) handleNotification(stream protocol.UI_NotificationsClient, notification *protocol.Notification) {
	switch {
	case notification.Type == protocol.Action_TASK_START:
		c.handleActionTaskStart(stream, notification)

	case notification.Type == protocol.Action_TASK_STOP:
		c.handleActionTaskStop(stream, notification)

	case notification.Type == protocol.Action_CHANGE_CONFIG:
		c.handleActionChangeConfig(stream, notification)

	case notification.Type == protocol.Action_ENABLE_INTERCEPTION:
		c.handleActionEnableInterception(stream, notification)

	case notification.Type == protocol.Action_DISABLE_INTERCEPTION:
		c.handleActionDisableInterception(stream, notification)

	case notification.Type == protocol.Action_RELOAD_FW_RULES:
		c.handleActionReloadFw(stream, notification)

	// ENABLE_RULE just replaces the rule on disk
	case notification.Type == protocol.Action_ENABLE_RULE:
		c.handleActionEnableRule(stream, notification)

	case notification.Type == protocol.Action_DISABLE_RULE:
		c.handleActionDisableRule(stream, notification)

	case notification.Type == protocol.Action_DELETE_RULE:
		c.handleActionDeleteRule(stream, notification)

	// CHANGE_RULE can add() or replace() an existing rule.
	case notification.Type == protocol.Action_CHANGE_RULE:
		c.handleActionChangeRule(stream, notification)
	}
}

func (c *Client) sendNotificationReply(stream protocol.UI_NotificationsClient, nID uint64, data string, err error) error {
	reply := NewReply(nID, protocol.NotificationReplyCode_OK, data)
	if err != nil {
		reply.Code = protocol.NotificationReplyCode_ERROR
		reply.Data = fmt.Sprint(err)
	}
	if err := stream.Send(reply); err != nil {
		log.Error("Error replying to notification: %s %d", err, reply.Id)
		return err
	}

	return nil
}

// Subscribe opens a connection with the server (UI), to start
// receiving notifications.
// It firstly sends the daemon status and configuration.
func (c *Client) Subscribe() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	clientCfg, err := c.client.Subscribe(ctx, c.getClientConfig())
	if err != nil {
		log.Error("Subscribing to GUI %s", err)
		// When connecting to the GUI via TCP, sometimes the notifications channel is
		// not established, and the main channel is never closed.
		// We need to disconnect everything after a timeout and try it again.
		c.disconnect()
		return
	}

	if tempConf, err := config.Parse(clientCfg.Config); err == nil {
		c.Lock()
		clientConnectedRule.Action = rule.Action(tempConf.DefaultAction)
		c.Unlock()
	}
	c.listenForNotifications()
}

// Notifications is the channel where the daemon receives messages from the server.
// It consists of 2 grpc streams (send/receive) that are never closed,
// this way we can share messages in realtime.
// If the GUI is closed, we'll receive an error reading from the channel.
func (c *Client) listenForNotifications() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// open the stream channel
	streamReply := &protocol.NotificationReply{Id: 0, Code: protocol.NotificationReplyCode_OK}
	notisStream, err := c.client.Notifications(ctx)
	if err != nil {
		log.Error("establishing notifications channel %s", err)
		return
	}
	// send the first notification
	if err := notisStream.Send(streamReply); err != nil {
		log.Error("sending notification HELLO %s", err)
		return
	}
	log.Info("Start receiving notifications")
	for {
		select {
		case <-c.clientCtx.Done():
			goto Exit
		default:
			noti, err := notisStream.Recv()
			if err == io.EOF {
				log.Warning("notification channel closed by the server")
				goto Exit
			}
			if err != nil {
				log.Error("getting notifications: %s %s", err, noti)
				goto Exit
			}
			c.handleNotification(notisStream, noti)
		}
	}
Exit:
	notisStream.CloseSend()
	log.Info("Stop receiving notifications")
	c.disconnect()
	TaskMgr.StopAll()
}
