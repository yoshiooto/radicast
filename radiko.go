package main

// api for radiko, rtmpdump and ffmpeg command parameter
// are taken from
// https://github.com/miyagawa/ripdiko
// https://gist.github.com/saiten/875864

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	radikoTimeLayout = "20060102150405"
	playerUrl        = "http://radiko.jp/apps/js/flash/myplayer-release.swf"
)

type RadikoPrograms struct {
	Stations struct {
		Station []struct {
			Id   string `xml:"id,attr"`
			Name string `xml:"name"`
			Scd  struct {
				Progs struct {
					Date string       `xml:"date"`
					Prog []RadikoProg `xml:"prog"`
				} `xml:"progs"`
			} `xml:"scd"`
		} `xml:"station"`
	} `xml:"stations"`
}

type RadikoProg struct {
	XMLName  xml.Name `xml:"prog"`
	Ft       string   `xml:"ft,attr"`
	To       string   `xml:"to,attr"`
	Ftl      string   `xml:"ftl,attr"`
	Tol      string   `xml:"tol,attr"`
	Dur      string   `xml:"dur,attr"`
	Title    string   `xml:"title"`
	Subtitle string   `xml:"subtitle"`
	Pfm      string   `xml:"pfm"`
	Desc     string   `xml:"desc"`
	Info     string   `xml:"info"`
	Url      string   `xml:"url"`
}

func (r *RadikoProg) FtTime() (time.Time, error) {
	return time.ParseInLocation(radikoTimeLayout, r.Ft, time.Local)
}

func (r *RadikoProg) ToTime() (time.Time, error) {
	return time.ParseInLocation(radikoTimeLayout, r.To, time.Local)
}

func (r *RadikoProg) Duration() (int64, error) {
	to, err := r.ToTime()
	if err != nil {
		return 0, err
	}
	return to.Unix() - time.Now().Unix(), nil
}

type RadikoResult struct {
	Mp3Path string
	Prog    *RadikoProg
	Station string
}

func (r *RadikoResult) Save(dir string) error {
	programDir := filepath.Join(dir, fmt.Sprintf("%s_%s", r.Prog.Ft, r.Station))

	if err := os.MkdirAll(programDir, 0777); err != nil {
		return err
	}

	m4aPath := filepath.Join(programDir, "podcast.m4a")
	xmlPath := filepath.Join(programDir, "podcast.xml")

	if err := RenameOrCopy(r.Mp3Path, m4aPath); err != nil {
		return err
	}

	xmlFile, err := os.Create(xmlPath)

	if err != nil {
		return err
	}

	defer xmlFile.Close()

	enc := xml.NewEncoder(xmlFile)
	enc.Indent("", "    ")
	if err := enc.Encode(r.Prog); err != nil {
		return err
	}

	r.Log("saved m4a:", m4aPath, " xml:", xmlPath)

	return nil
}

func (r *RadikoResult) Log(v ...interface{}) {
	log.Println("[radiko_result]", fmt.Sprint(v...))
}

type Radiko struct {
	Station   string
	Bitrate   string
	Buffer    int64
	Converter string
	TempDir   string
	Result    *RadikoResult
}

func (r *Radiko) Run(ctx context.Context) error {

	results := r.run(ctx)

	switch len(results) {
	case 0:
		return fmt.Errorf("empty outputs")
	case 1:
		r.Result = results[0]
		return nil
	default:
		result, err := r.ConcatOutput(r.TempDir, results)
		if err != nil {
			return err
		}
		r.Result = result
		return nil
	}
}

func (r *Radiko) run(ctx context.Context) []*RadikoResult {
	errChan := make(chan error)

	retry := 0
	c := make(chan struct{}, 1)

	results := []*RadikoResult{}

	record := func() error {
		output := filepath.Join(r.TempDir, fmt.Sprintf("radiko_%d.m4a", retry))

		ret, err := r.record(ctx, output, r.Station, r.Bitrate, r.Buffer)

		if ret != nil {
			results = append(results, ret)
		}

		return err
	}

	c <- struct{}{}

	for {
		select {
		case <-c:
			r.Log("start record")
			go func() {
				errChan <- record()
			}()
		case <-ctx.Done():
			if err := ctx.Err(); err != nil {
				r.Log("context err:", err)
			}

			select {
			case err := <-errChan:
				r.Log("err:", err)
			case <-time.After(time.Second * 10):
				r.Log("timeout receive err chan")
			}
			return results
		case err := <-errChan:
			r.Log("finished")
			if err == nil {
				return results
			}

			// TODO stop if recod program is changed.
			r.Log("got err:", err)
			if retry < 5 {
				sec := time.Second * 10
				time.AfterFunc(sec, func() {
					c <- struct{}{}
				})
				r.Log("retry after ", sec)
				retry++
			} else {
				return results
			}
		}
	}
}

// http://superuser.com/questions/314239/how-to-join-merge-many-m4a-files
func (r *Radiko) ConcatOutput(dir string, results []*RadikoResult) (*RadikoResult, error) {
	output := filepath.Join(dir, "radiko_concat.m4a")

	outputs := []string{}
	for _, result := range results {
		outputs = append(outputs, result.Mp3Path)
	}

	args := []string{
		"-i",
		fmt.Sprintf("concat:%s", strings.Join(outputs, "|")),
		"-acodec",
		"copy",
		output,
	}

	cmd := exec.Command(r.Converter, args...)
	r.Log("concat cmd:", strings.Join(cmd.Args, " "))

	if err := cmd.Run(); err != nil {
		return nil, err
	}

	return &RadikoResult{
		Mp3Path: output,
		Station: results[0].Station,
		Prog:    results[0].Prog,
	}, nil
}

func (r *Radiko) StationList(ctx context.Context) ([]string, error) {
	_, area, err := r.auth(ctx)
	if err != nil {
		return nil, err
	}

	progs, err := r.todayPrograms(ctx, area)
	if err != nil {
		return nil, err
	}

	stations := make([]string, len(progs.Stations.Station))

	for i, station := range progs.Stations.Station {
		stations[i] = station.Id
	}

	return stations, nil
}

func (r *Radiko) todayPrograms(ctx context.Context, area string) (*RadikoPrograms, error) {
	u, err := url.Parse("http://radiko.jp/v2/api/program/today")

	if err != nil {
		return nil, err
	}

	v := u.Query()
	v.Set("area_id", area)

	u.RawQuery = v.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)

	if err != nil {
		return nil, err
	}

	var progs RadikoPrograms
	err = r.httpDo(ctx, req, func(resp *http.Response, err error) error {
		if err != nil {
			return err
		}

		defer resp.Body.Close()

		if code := resp.StatusCode; code != 200 {
			return fmt.Errorf("not status code:200, got:%d", code)
		}

		return xml.NewDecoder(resp.Body).Decode(&progs)
	})

	if err != nil {
		return nil, err
	}

	return &progs, nil
}

func (r *Radiko) nowProgram(ctx context.Context, area string, station string) (*RadikoProg, error) {
	progs, err := r.todayPrograms(ctx, area)

	if err != nil {
		return nil, err
	}

	for _, s := range progs.Stations.Station {
		if s.Id == station {
			for _, prog := range s.Scd.Progs.Prog {
				ft, err := prog.FtTime()
				if err != nil {
					return nil, err
				}

				to, err := prog.ToTime()
				if err != nil {
					return nil, err
				}

				now := time.Now()

				if ft.Unix() <= now.Unix() && now.Unix() < to.Unix() {
					return &prog, nil
				}
			}
		}
	}

	return nil, errors.New("not found program")
}

func (r *Radiko) record(ctx context.Context, output string, station string, bitrate string, buffer int64) (*RadikoResult, error) {

	authtoken, area, err := r.auth(ctx)

	if err != nil {
		return nil, err
	}

	prog, err := r.nowProgram(ctx, area, station)

	if err != nil {
		return nil, err
	}

	r.Log("start recording ", prog.Title)

	duration, err := prog.Duration()

	if err != nil {
		return nil, err
	}

	duration += buffer

	ft, err := prog.FtTime()

	if err != nil {
		return nil, err
	}

	title := fmt.Sprintf("%s (%s)", prog.Title, ft)

	err = r.download(ctx, authtoken, station, fmt.Sprint(duration), bitrate, output, title, prog.Pfm)

	if _, fileErr := os.Stat(output); fileErr != nil {
		return nil, err
	}

	ret := &RadikoResult{
		Mp3Path: output,
		Station: station,
		Prog:    prog,
	}

	return ret, err
}

func (r *Radiko) download(ctx context.Context, authtoken string, station string, sec string, bitrate string, output string, title string, author string) error {

	rtmpdump, err := exec.LookPath("rtmpdump")

	if err != nil {
		return err
	}

	rtmpdumpCmd := exec.Command(rtmpdump,
		"--live",
		"--quiet",
		"-r", "rtmpe://f-radiko.smartstream.ne.jp",
		"--playpath", "simul-stream.stream",
		"--app", station+"/_definst_",
		"-W", playerUrl,
		"-C", `S:""`, "-C", `S:""`, "-C", `S:""`, "-C", "S:"+authtoken,
		"--stop", sec,
		"-o", "-",
	)

	converterCmd, err := newConverterCmd(r.Converter, bitrate, output, title, author)

	if err != nil {
		return err
	}

	r.Log("rtmpdump command: ", strings.Join(rtmpdumpCmd.Args, " "))
	r.Log("converter command: ", strings.Join(converterCmd.Args, " "))

	pipe, err := rtmpdumpCmd.StdoutPipe()

	if err != nil {
		return err
	}

	converterCmd.Stdin = pipe

	errChan := make(chan error)
	go func() {

		if err := converterCmd.Start(); err != nil {
			errChan <- err
			return
		}

		if err := rtmpdumpCmd.Run(); err != nil {
			errChan <- err
			return
		}

		if err := converterCmd.Wait(); err != nil {
			errChan <- err
			return
		}

		errChan <- nil
	}()

	select {
	case <-ctx.Done():
		rtmpdumpCmd.Process.Kill()
		err := <-errChan
		if err == nil {
			err = ctx.Err()
		}
		return err
	case err := <-errChan:
		return err
	}

	return nil
}

// return authtoken, area, err
func (r *Radiko) auth(ctx context.Context) (string, string, error) {
	req, err := http.NewRequest("GET", playerUrl, nil)

	if err != nil {
		return "", "", err
	}

	tmpSwfFile, err := ioutil.TempFile("", "swf")

	if err != nil {
		return "", "", err
	}

	defer func() {
		tmpSwfFile.Close()
		os.Remove(tmpSwfFile.Name())
	}()

	err = r.httpDo(ctx, req, func(resp *http.Response, err error) error {
		if err != nil {
			return err
		}

		defer resp.Body.Close()

		if _, err := io.Copy(tmpSwfFile, resp.Body); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return "", "", err
	}

	swfextract, err := exec.LookPath("swfextract")

	if err != nil {
		return "", "", err
	}

	tmpAuthKeyPngFile, err := ioutil.TempFile("", ".png")

	if err != nil {
		return "", "", err
	}

	defer func() {
		tmpAuthKeyPngFile.Close()
		os.Remove(tmpAuthKeyPngFile.Name())
	}()

	swfextractCmd := exec.Command(swfextract, "-b", "12", tmpSwfFile.Name(), "-o", tmpAuthKeyPngFile.Name())
	if err := swfextractCmd.Run(); err != nil {
		return "", "", err
	}

	req, err = http.NewRequest("POST", "https://radiko.jp/v2/api/auth1_fms", nil)

	if err != nil {
		return "", "", err
	}

	req.Header.Set("pragma", "no-cache")
	req.Header.Set("X-Radiko-App", "pc_ts")
	req.Header.Set("X-Radiko-App-Version", "4.0.0")
	req.Header.Set("X-Radiko-User", "test-stream")
	req.Header.Set("X-Radiko-Device", "pc")

	var authtoken string
	var partialkey string

	err = r.httpDo(ctx, req, func(resp *http.Response, err error) error {
		if err != nil {
			return err
		}

		defer resp.Body.Close()

		authtoken = resp.Header.Get("X-Radiko-Authtoken")
		keylength := resp.Header.Get("X-Radiko-Keylength")
		keyoffset := resp.Header.Get("X-Radiko-Keyoffset")

		if authtoken == "" {
			return errors.New("auth token is empty")
		}

		if keylength == "" {
			return errors.New("keylength is empty")
		}

		if keyoffset == "" {
			return errors.New("keyoffset is empty")
		}

		keylengthI, err := strconv.Atoi(keylength)

		if err != nil {
			return err
		}

		keyoffsetI, err := strconv.Atoi(keyoffset)

		if err != nil {
			return err
		}

		partialkeyByt := make([]byte, keylengthI)
		if _, err = tmpAuthKeyPngFile.ReadAt(partialkeyByt, int64(keyoffsetI)); err != nil {
			return err
		}

		partialkey = base64.StdEncoding.EncodeToString(partialkeyByt)

		return nil
	})

	if err != nil {
		return "", "", err
	}

	req, err = http.NewRequest("POST", "https://radiko.jp/v2/api/auth2_fms", nil)

	if err != nil {
		return "", "", err
	}

	req.Header.Set("pragma", "no-cache")
	req.Header.Set("X-Radiko-App", "pc_ts")
	req.Header.Set("X-Radiko-App-Version", "4.0.0")
	req.Header.Set("X-Radiko-User", "test-stream")
	req.Header.Set("X-Radiko-Device", "pc")
	req.Header.Set("X-Radiko-Authtoken", authtoken)
	req.Header.Set("X-Radiko-Partialkey", partialkey)

	var area string
	err = r.httpDo(ctx, req, func(resp *http.Response, err error) error {
		if err != nil {
			return err
		}

		defer resp.Body.Close()

		byt, err := ioutil.ReadAll(resp.Body)

		if err != nil {
			return err
		}

		matches := regexp.MustCompile("(.*),(.*),(.*)").FindAllStringSubmatch(string(byt), -1)

		if len(matches) == 1 && len(matches[0]) != 4 {
			return errors.New("failed to auth")
		}

		area = matches[0][1]

		return nil
	})

	if err != nil {
		return "", "", err
	}

	return authtoken, area, nil
}

func (r *Radiko) Log(v ...interface{}) {
	log.Println("[radiko]", fmt.Sprint(v...))
}

// http://blog.golang.org/context/google/google.go
func (r *Radiko) httpDo(ctx context.Context, req *http.Request, f func(*http.Response, error) error) error {
	r.Log(req.Method + " " + req.URL.String())

	errChan := make(chan error)

	go func() { errChan <- f(http.DefaultClient.Do(req)) }()

	select {
	case <-ctx.Done():
		http.DefaultTransport.(*http.Transport).CancelRequest(req)
		err := <-errChan
		if err == nil {
			err = ctx.Err()
		}
		return err
	case err := <-errChan:
		return err
	}
}
