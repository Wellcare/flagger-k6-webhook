package handlers

import (
	"errors"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/grafana/flagger-k6-webhook/pkg/k6"
	"github.com/grafana/flagger-k6-webhook/pkg/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewLaunchPayload(t *testing.T) {
	testCases := []struct {
		name    string
		request *http.Request
		want    *launchPayload
		wantErr error
	}{
		{
			name:    "no data",
			request: &http.Request{},
			wantErr: errors.New("no request body"),
		},
		{
			name: "invalid json",
			request: &http.Request{
				Body: ioutil.NopCloser(strings.NewReader("bad")),
			},
			wantErr: errors.New("invalid character 'b' looking for beginning of value"),
		},
		{
			name: "missing base webhook attributes",
			request: &http.Request{
				Body: ioutil.NopCloser(strings.NewReader("{}")),
			},
			wantErr: errors.New("error while validating base webhook: missing name"),
		},
		{
			name: "missing script",
			request: &http.Request{
				Body: ioutil.NopCloser(strings.NewReader(`{"name": "test", "namespace": "test", "phase": "pre-rollout", "metadata": {}}`)),
			},
			wantErr: errors.New("missing script"),
		},
		{
			name: "default values",
			request: &http.Request{
				Body: ioutil.NopCloser(strings.NewReader(`{"name": "test", "namespace": "test", "phase": "pre-rollout", "metadata": {"script": "my-script"}}`)),
			},
			want: func() *launchPayload {
				p := &launchPayload{flaggerWebhook: flaggerWebhook{Name: "test", Namespace: "test", Phase: "pre-rollout"}}
				p.Metadata.Script = "my-script"
				p.Metadata.UploadToCloud = false
				p.Metadata.WaitForResults = true
				p.Metadata.SlackChannels = nil
				return p
			}(),
		},
		{
			name: "set values",
			request: &http.Request{
				Body: ioutil.NopCloser(strings.NewReader(`{"name": "test", "namespace": "test", "phase": "pre-rollout", "metadata": {"script": "my-script", "upload_to_cloud": "true", "wait_for_results": "false", "slack_channels": "test,test2"}}`)),
			},
			want: func() *launchPayload {
				p := &launchPayload{flaggerWebhook: flaggerWebhook{Name: "test", Namespace: "test", Phase: "pre-rollout"}}
				p.Metadata.Script = "my-script"
				p.Metadata.UploadToCloudString = "true"
				p.Metadata.UploadToCloud = true
				p.Metadata.WaitForResultsString = "false"
				p.Metadata.WaitForResults = false
				p.Metadata.SlackChannelsString = "test,test2"
				p.Metadata.SlackChannels = []string{"test", "test2"}
				return p
			}(),
		},
		{
			name: "invalid upload_to_cloud",
			request: &http.Request{
				Body: ioutil.NopCloser(strings.NewReader(`{"name": "test", "namespace": "test", "phase": "pre-rollout", "metadata": {"script": "my-script", "upload_to_cloud": "bad"}}`)),
			},
			wantErr: errors.New(`error parsing value for 'upload_to_cloud': strconv.ParseBool: parsing "bad": invalid syntax`),
		},
		{
			name: "invalid wait_for_results",
			request: &http.Request{
				Body: ioutil.NopCloser(strings.NewReader(`{"name": "test", "namespace": "test", "phase": "pre-rollout", "metadata": {"script": "my-script", "wait_for_results": "bad"}}`)),
			},
			wantErr: errors.New(`error parsing value for 'wait_for_results': strconv.ParseBool: parsing "bad": invalid syntax`),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := newLaunchPayload(tc.request)
			if tc.wantErr != nil {
				assert.EqualError(t, err, tc.wantErr.Error())
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.want, payload)
		})
	}
}

func TestLaunchAndWaitCloud(t *testing.T) {
	// Initialize controller
	mockCtrl := gomock.NewController(t)
	k6Client := mocks.NewMockK6Client(mockCtrl)
	slackClient := mocks.NewMockSlackClient(mockCtrl)
	testRun := mocks.NewMockK6TestRun(mockCtrl)
	handler, err := NewLaunchHandler(k6Client, slackClient)
	handler.(*launchHandler).sleep = func(d time.Duration) {}
	require.NoError(t, err)

	// Expected calls
	// * Start the run
	fullResults, err := os.ReadFile("testdata/k6-output.txt")
	resultParts := strings.SplitN(string(fullResults), "running", 2)
	var bufferWriter io.Writer
	require.NoError(t, err)
	k6Client.EXPECT().Start("my-script", true, gomock.Any()).DoAndReturn(func(scriptContent string, upload bool, outputWriter io.Writer) (k6.TestRun, error) {
		bufferWriter = outputWriter
		outputWriter.Write([]byte(resultParts[0]))
		return testRun, nil
	})

	// * Send the initial slack message
	channelMap := map[string]string{"C1234": "ts1", "C12345": "ts2"}
	slackClient.EXPECT().SendMessages(
		[]string{"test", "test2"},
		":warning: Load testing of `test-name` in namespace `test-space` has started",
		"Cloud URL: <https://app.k6.io/runs/1157843>",
	).Return(channelMap, nil)

	// * Wait for the command to finish
	testRun.EXPECT().Wait().DoAndReturn(func() error {
		bufferWriter.Write([]byte("running" + resultParts[1]))
		return nil
	})

	// * Upload the results file and update the slack message
	slackClient.EXPECT().AddFileToThreads(
		channelMap,
		"k6-results.txt",
		string(fullResults),
	).Return(nil)
	slackClient.EXPECT().UpdateMessages(
		channelMap,
		":large_green_circle: Load testing of `test-name` in namespace `test-space` has succeeded",
		"Cloud URL: <https://app.k6.io/runs/1157843>",
	).Return(nil)

	// Make request
	request := &http.Request{
		Body: ioutil.NopCloser(strings.NewReader(`{"name": "test-name", "namespace": "test-space", "phase": "pre-rollout", "metadata": {"script": "my-script", "upload_to_cloud": "true", "slack_channels": "test,test2"}}`)),
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	// Expected response
	assert.Equal(t, "", rr.Body.String())
	assert.Equal(t, 200, rr.Result().StatusCode)
}

func TestLaunchAndWaitLocal(t *testing.T) {
	// Initialize controller
	mockCtrl := gomock.NewController(t)
	k6Client := mocks.NewMockK6Client(mockCtrl)
	slackClient := mocks.NewMockSlackClient(mockCtrl)
	testRun := mocks.NewMockK6TestRun(mockCtrl)
	handler, err := NewLaunchHandler(k6Client, slackClient)
	handler.(*launchHandler).sleep = func(d time.Duration) {}
	require.NoError(t, err)

	// Expected calls
	// * Start the run
	fullResults, err := os.ReadFile("testdata/k6-output.txt")
	resultParts := strings.SplitN(string(fullResults), "running", 2)
	var bufferWriter io.Writer
	require.NoError(t, err)
	k6Client.EXPECT().Start("my-script", false, gomock.Any()).DoAndReturn(func(scriptContent string, upload bool, outputWriter io.Writer) (k6.TestRun, error) {
		bufferWriter = outputWriter
		outputWriter.Write([]byte(resultParts[0]))
		return testRun, nil
	})

	// * Send the initial slack message
	channelMap := map[string]string{"C1234": "ts1", "C12345": "ts2"}
	slackClient.EXPECT().SendMessages(
		[]string{"test", "test2"},
		":warning: Load testing of `test-name` in namespace `test-space` has started",
		"",
	).Return(channelMap, nil)

	// * Wait for the command to finish
	testRun.EXPECT().Wait().DoAndReturn(func() error {
		bufferWriter.Write([]byte("running" + resultParts[1]))
		return nil
	})

	// * Upload the results file and update the slack message
	slackClient.EXPECT().AddFileToThreads(
		channelMap,
		"k6-results.txt",
		string(fullResults),
	).Return(nil)
	slackClient.EXPECT().UpdateMessages(
		channelMap,
		":large_green_circle: Load testing of `test-name` in namespace `test-space` has succeeded",
		"",
	).Return(nil)

	// Make request
	request := &http.Request{
		Body: ioutil.NopCloser(strings.NewReader(`{"name": "test-name", "namespace": "test-space", "phase": "pre-rollout", "metadata": {"script": "my-script", "upload_to_cloud": "false", "slack_channels": "test,test2"}}`)),
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	// Expected response
	assert.Equal(t, "", rr.Body.String())
	assert.Equal(t, 200, rr.Result().StatusCode)
}

func TestLaunchAndWaitAndGetError(t *testing.T) {
	// Initialize controller
	mockCtrl := gomock.NewController(t)
	k6Client := mocks.NewMockK6Client(mockCtrl)
	slackClient := mocks.NewMockSlackClient(mockCtrl)
	testRun := mocks.NewMockK6TestRun(mockCtrl)
	handler, err := NewLaunchHandler(k6Client, slackClient)
	handler.(*launchHandler).sleep = func(d time.Duration) {}
	require.NoError(t, err)

	// Expected calls
	// * Start the run
	fullResults, err := os.ReadFile("testdata/k6-output.txt")
	resultParts := strings.SplitN(string(fullResults), "running", 2)
	var bufferWriter io.Writer
	require.NoError(t, err)
	k6Client.EXPECT().Start("my-script", false, gomock.Any()).DoAndReturn(func(scriptContent string, upload bool, outputWriter io.Writer) (k6.TestRun, error) {
		bufferWriter = outputWriter
		outputWriter.Write([]byte(resultParts[0]))
		return testRun, nil
	})

	// * Send the initial slack message
	channelMap := map[string]string{"C1234": "ts1", "C12345": "ts2"}
	slackClient.EXPECT().SendMessages(
		[]string{"test", "test2"},
		":warning: Load testing of `test-name` in namespace `test-space` has started",
		"",
	).Return(channelMap, nil)

	// * Wait for the command to finish
	testRun.EXPECT().Wait().DoAndReturn(func() error {
		bufferWriter.Write([]byte("running" + resultParts[1]))
		return errors.New("exit code 1")
	})

	// * Upload the results file and update the slack message
	slackClient.EXPECT().AddFileToThreads(
		channelMap,
		"k6-results.txt",
		string(fullResults),
	).Return(nil)
	slackClient.EXPECT().UpdateMessages(
		channelMap,
		":red_circle: Load testing of `test-name` in namespace `test-space` has failed",
		"",
	).Return(nil)

	// Make request
	request := &http.Request{
		Body: ioutil.NopCloser(strings.NewReader(`{"name": "test-name", "namespace": "test-space", "phase": "pre-rollout", "metadata": {"script": "my-script", "upload_to_cloud": "false", "slack_channels": "test,test2"}}`)),
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	// Expected response
	assert.Equal(t, "failed to run: exit code 1\n", rr.Body.String())
	assert.Equal(t, 400, rr.Result().StatusCode)
}

func TestLaunchNeverStarted(t *testing.T) {
	var sleepCalls []time.Duration
	sleepMock := func(d time.Duration) {
		sleepCalls = append(sleepCalls, d)
	}

	// Initialize controller
	mockCtrl := gomock.NewController(t)
	k6Client := mocks.NewMockK6Client(mockCtrl)
	slackClient := mocks.NewMockSlackClient(mockCtrl)
	testRun := mocks.NewMockK6TestRun(mockCtrl)
	handler, err := NewLaunchHandler(k6Client, slackClient)
	handler.(*launchHandler).sleep = sleepMock
	require.NoError(t, err)

	// Expected calls
	// * Start the run (process fails and prints out an error)
	require.NoError(t, err)
	k6Client.EXPECT().Start("my-script", false, gomock.Any()).DoAndReturn(func(scriptContent string, upload bool, outputWriter io.Writer) (k6.TestRun, error) {
		outputWriter.Write([]byte("failed to run (k6 error)"))
		return testRun, nil
	})

	// * Upload the results file and send the error slack message
	channelMap := map[string]string{"C1234": "ts1", "C12345": "ts2"}
	slackClient.EXPECT().SendMessages(
		[]string{"test", "test2"},
		":red_circle: Load testing of `test-name` in namespace `test-space` didn't start successfully",
		"",
	).Return(channelMap, nil)
	slackClient.EXPECT().AddFileToThreads(
		channelMap,
		"k6-results.txt",
		"failed to run (k6 error)",
	).Return(nil)

	// Make request
	request := &http.Request{
		Body: ioutil.NopCloser(strings.NewReader(`{"name": "test-name", "namespace": "test-space", "phase": "pre-rollout", "metadata": {"script": "my-script", "upload_to_cloud": "false", "slack_channels": "test,test2"}}`)),
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	// Expected response
	assert.Equal(t, "error while waiting for test to start: timeout\n", rr.Body.String())
	assert.Equal(t, 400, rr.Result().StatusCode)
	// 10 sleep calls
	assert.Equal(t, sleepCalls, []time.Duration{2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second,
		2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second})
}

func TestLaunchWithoutWaiting(t *testing.T) {
	// Initialize controller
	mockCtrl := gomock.NewController(t)
	k6Client := mocks.NewMockK6Client(mockCtrl)
	slackClient := mocks.NewMockSlackClient(mockCtrl)
	testRun := mocks.NewMockK6TestRun(mockCtrl)
	handler, err := NewLaunchHandler(k6Client, slackClient)
	handler.(*launchHandler).sleep = func(d time.Duration) {}
	require.NoError(t, err)

	// Expected calls
	// * Start the run
	fullResults, err := os.ReadFile("testdata/k6-output.txt")
	resultParts := strings.SplitN(string(fullResults), "running", 2)
	require.NoError(t, err)
	k6Client.EXPECT().Start("my-script", false, gomock.Any()).DoAndReturn(func(scriptContent string, upload bool, outputWriter io.Writer) (k6.TestRun, error) {
		outputWriter.Write([]byte(resultParts[0]))
		return testRun, nil
	})

	// * Send the initial slack message (process ends here)
	channelMap := map[string]string{"C1234": "ts1", "C12345": "ts2"}
	slackClient.EXPECT().SendMessages(
		[]string{"test", "test2"},
		":warning: Load testing of `test-name` in namespace `test-space` has started",
		"",
	).Return(channelMap, nil)

	// Make request
	request := &http.Request{
		Body: ioutil.NopCloser(strings.NewReader(`{"name": "test-name", "namespace": "test-space", "phase": "pre-rollout", "metadata": {"script": "my-script", "wait_for_results": "false", "slack_channels": "test,test2"}}`)),
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	// Expected response
	assert.Equal(t, "", rr.Body.String())
	assert.Equal(t, 200, rr.Result().StatusCode)
}