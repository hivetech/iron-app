package main

import (
  "fmt"
  "os"
  "os/exec"
  "reflect"
  "regexp"
  "strings"
  "syscall"
  "time"

  "github.com/Sirupsen/logrus"
  "github.com/armon/consul-api"

  "github.com/hivetech/iron-app/log"
)

// Configuration for watches.
type WatchConfig struct {
  ConsulAddr string
  ConsulDC   string
  Cmd        []string
  ErrExit    bool
  Prefix     string
  Reload     bool
  Sanitize   bool
  Upcase     bool
}

var (
  // Regexp for invalid characters in keys
  InvalidRegexp = regexp.MustCompile(`[^a-zA-Z0-9_]`)
)

// Connects to Consul and watches a given K/V prefix and uses that to
// execute a child process.
func watchAndExec(config *WatchConfig) (int, error) {
  kvConfig := consulapi.DefaultConfig()
  kvConfig.Address = config.ConsulAddr
  kvConfig.Datacenter = config.ConsulDC

  Log.WithFields(logrus.Fields{
    "address":    kvConfig.Address,
    "datacenter": kvConfig.Datacenter,
  }).Info("Connecting to consul.")
  client, err := consulapi.NewClient(kvConfig)
  if err != nil {
    return 0, err
  }

  // Start the watcher goroutine that watches for changes in the
  // K/V and notifies us on a channel.
  errCh := make(chan error, 1)
  pairCh := make(chan consulapi.KVPairs)
  quitCh := make(chan struct{})
  defer close(quitCh)
  go watch(
    client, config.Prefix, pairCh, errCh, quitCh,
    config.ErrExit, config.Reload)

  // This channel is what is sent to when a process exits that we
  // are running. We start it out as `nil` since we have no process.
  var exitCh chan int

  var env map[string]string
  var cmd *exec.Cmd
  for {
    var pairs consulapi.KVPairs

    // Wait for new pairs to come on our channel or an error
    // to occur.
    select {
    case exit := <-exitCh:
      return exit, nil
    case pairs = <-pairCh:
    case err := <-errCh:
      return 0, err
    }

    newEnv := make(map[string]string)
    for _, pair := range pairs {
      k := strings.TrimPrefix(pair.Key, config.Prefix)
      k = strings.TrimLeft(k, "/")
      if config.Sanitize {
        k = InvalidRegexp.ReplaceAllString(k, "_")
      }
      if config.Upcase {
        k = strings.ToUpper(k)
      }

      Log.WithFields(logrus.Fields{
        "key":   k,
        "value": string(pair.Value),
      }).Info("Fetched environment variable.")
      newEnv[k] = string(pair.Value)
    }

    // If the environmental variables didn't actually change,
    // then don't do anything.
    if reflect.DeepEqual(env, newEnv) {
      Log.Debug("Nothing new in KV store.")
      continue
    }

    // Replace the env so we can detect future changes
    env = newEnv

    Log.Info("Configuration changed, reload the process.")
    if cmd != nil {
      if !config.Reload {
        Log.Info("We don't want to reload the process... just ignore.")
        continue
      }

      Log.Info("Kill the process.")
      exited := false
      if err := cmd.Process.Signal(syscall.SIGTERM); err == nil {
        // Wait a few seconds for it to exit
        killCh := make(chan struct{})
        go func() {
          defer close(killCh)
          cmd.Process.Wait()
        }()

        select {
        case <-killCh:
          exited = true
        case <-time.After(3 * time.Second):
        }
      }

      // If we still haven't exited from a SIGKILL
      if !exited {
        cmd.Process.Kill()
      }

      cmd = nil
    }

    Log.Info("Loading variables into process env.")
    processEnv := os.Environ()
    cmdEnv := make(
      []string, len(processEnv), len(newEnv)+len(processEnv))
    copy(cmdEnv, processEnv)
    for k, v := range newEnv {
      cmdEnv = append(cmdEnv, fmt.Sprintf("%s=%s", k, v))
    }
    cmd = exec.Command(config.Cmd[0], config.Cmd[1:]...)
    cmd.Stdout = log.NewLogstream(Log, "stdout", config.Cmd[0])
    cmd.Stderr = log.NewLogstream(Log, "stderr", config.Cmd[0])

    Log.Info("Starting process ...")
    cmd.Env = cmdEnv
    err := cmd.Start()
    if err != nil {
      return 111, err
    }

    // Create a new exitCh so that previously invoked commands
    // (if any) don't cause us to exit, and start a goroutine
    // to wait for that process to end.
    exitCh = make(chan int, 1)
    go func(cmd *exec.Cmd, exitCh chan<- int) {
      err := cmd.Wait()
      if err == nil {
        exitCh <- 0
        return
      }

      if exiterr, ok := err.(*exec.ExitError); ok {
        // The program has exited with an exit code != 0
        if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
          exitCh <- status.ExitStatus()
          return
        }
      }

      exitCh <- 111
    }(cmd, exitCh)
  }

  return 0, nil
}

func watch(
  client *consulapi.Client,
  prefix string,
  pairCh chan<- consulapi.KVPairs,
  errCh chan<- error,
  quitCh <-chan struct{},
  errExit bool,
  watch bool) {
  // Get the initial list of k/v pairs. We don't do a retryableList
  // here because we want a fast fail if the initial request fails.
  pairs, meta, err := client.KV().List(prefix, nil)
  if err != nil {
    errCh <- err
    return
  }

  Log.Debug("Send the initial list out right away.")
  pairCh <- pairs

  // If we're not watching, just return right away
  if !watch {
    return
  }

  curIndex := meta.LastIndex
  Log.WithFields(logrus.Fields{
    "index": curIndex,
  }).Info("Loop forever and watch the keys for changes.")
  for {
    select {
    case <-quitCh:
      return
    default:
    }

    pairs, meta, err = retryableList(
      func() (consulapi.KVPairs, *consulapi.QueryMeta, error) {
        opts := &consulapi.QueryOptions{WaitIndex: curIndex}
        return client.KV().List(prefix, opts)
      })
    if err != nil {
      if errExit {
        errCh <- err
        return
      }
    }

    pairCh <- pairs
    curIndex = meta.LastIndex
  }
}

// This function is able to call KV listing functions and retry them.
// We want to retry if there are errors because it is safe (GET request),
// and erroring early is MUCH more costly than retrying over time and
// delaying the configuration propagation.
func retryableList(f func() (consulapi.KVPairs, *consulapi.QueryMeta, error)) (consulapi.KVPairs, *consulapi.QueryMeta, error) {
  i := 0
  for {
    p, m, e := f()
    if e != nil {
      if i >= 3 {
        return nil, nil, e
      }

      i++

      // Reasonably arbitrary sleep to just try again... It is
      // a GET request so this is safe.
      time.Sleep(time.Duration(i*2) * time.Second)
    }

    return p, m, e
  }
}
