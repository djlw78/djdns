package server

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"reflect"
	"testing"
	"time"

	"github.com/DJDNS/djdns/model"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
)

func TestNewServer(t *testing.T) {
	spgc := NewStandardPGConfig(nil)
	s := NewServer(spgc.Alias)
	if s.Port != 9953 {
		t.Fatalf("Expected port 9953, got %d", s.Port)
	}
}

type GetRecordsTest struct {
	Query       string
	Expected    []model.Record
	ErrorString string
	Description string
}

func (grt *GetRecordsTest) Run(t *testing.T, s DjdnsServer) {
	result, err := s.GetRecords(grt.Query)
	assertError(t, grt.ErrorString, err)
	if !reflect.DeepEqual(result, grt.Expected) {
		t.Log(grt.Query)
		t.Log(grt.Expected)
		t.Log(result)
		t.Fatal(grt.Description)
	}
}

func setupTestData(writer io.Writer) (DjdnsServer, StandardPGConfig) {
	spgc := NewStandardPGConfig(writer)
	s := NewServer(spgc.Alias)
	s.Logger = log.New(writer, "djdns: ", 0)

	root := DummyPageGetter{}
	root.PageData.Data.Branches = []model.Branch{
		model.Branch{
			Selector: "abc",
			Records: []model.Record{
				model.Record{
					DomainName: "first",
					Rdata:      "1.1.1.1",
				},
				model.Record{
					DomainName: "second",
					Rdata:      "2.2.2.2",
				},
			},
		},
		model.Branch{
			Selector: "evil",
			Records: []model.Record{
				model.Record{
					DomainName: "evil.record.",
					Rtype:      "EVIL",
					Rdata:      5,
				},
			},
		},
		model.Branch{
			Selector: "dog*",
			Targets:  []string{"secondary://"},
		},
		model.Branch{
			Selector: "slow*",
			Targets:  []string{"slow://"},
		},
	}
	root.PageData.Data.Normalize()

	secondary := DummyPageGetter{}
	secondary.PageData.Data.Branches = []model.Branch{
		model.Branch{
			Selector: "dogbreath",
			Records: []model.Record{
				model.Record{
					DomainName: "only.smells",
					Rdata:      "3.3.3.3",
				},
			},
		},
	}
	secondary.PageData.Data.Normalize()

	slow := SlowPageGetter(2 * time.Second)

	spgc.Alias.Aliases["<ROOT>"] = "root://"
	spgc.Alias.Aliases["secondary"] = "secondary://"
	spgc.Alias.Aliases["slow"] = "slow://"
	spgc.Scheme.Children["root"] = &root
	spgc.Scheme.Children["secondary"] = &secondary
	spgc.Scheme.Children["slow"] = slow
	return s, spgc
}

func Test_DjdnsServer_GetRecords(t *testing.T) {
	// Setup
	s, pg_config := setupTestData(nil)
	root := pg_config.Scheme.Children["root"].(*DummyPageGetter)
	secondary := pg_config.Scheme.Children["secondary"].(*DummyPageGetter)

	// Actual tests
	tests := []GetRecordsTest{
		GetRecordsTest{
			"abcde",
			root.PageData.Data.Branches[0].Records,
			"",
			"Basic request",
		},
		GetRecordsTest{
			"no such branch",
			nil,
			"",
			"Branch does not exist",
		},
		GetRecordsTest{
			"dogbreath.de",
			secondary.PageData.Data.Branches[0].Records,
			"",
			"Recursive request",
		},
		GetRecordsTest{
			"slow.query",
			nil,
			"Ran out of time",
			"Timeout failure",
		},
	}
	for i := range tests {
		tests[i].Run(t, s)
	}
}

type ResolveTest struct {
	Description     string
	Header          dns.MsgHdr
	QuestionSection []dns.Question
	ExpectedHeader  dns.MsgHdr
	ExpectedAnswers []string
	ShouldFail      bool
}

type ResolveTester interface {
	GetResponse(query *dns.Msg) (*dns.Msg, error)
	WasFailure(response *dns.Msg, err error) bool
}

func testResolution(t *testing.T, tester ResolveTester, rt ResolveTest) {
	t.Log(" => " + rt.Description)

	// Construct query
	query := new(dns.Msg)
	query.MsgHdr = rt.Header
	query.Question = rt.QuestionSection

	// Get response
	response, err := tester.GetResponse(query)
	was_failure := tester.WasFailure(response, err)
	if rt.ShouldFail {
		// Expecting a failure...
		// ...in fact, if we don't get one, that's BAD
		if !was_failure {
			t.Fatal("Test should have failed, didn't")
		}
		return
	} else {
		// Normal case, should not fail
		if was_failure {
			t.Log(rt)
			t.Fatal(err)
		}
	}

	// Construct expected response
	expected := new(dns.Msg)
	expected.MsgHdr = rt.ExpectedHeader
	expected.Question = query.Question
	expected.Answer = make([]dns.RR, len(rt.ExpectedAnswers))
	for i, answer := range rt.ExpectedAnswers {
		rr, err := dns.NewRR(answer)
		if err != nil {
			t.Fatal(err)
		}
		expected.Answer[i] = rr
	}
	expected.Ns = make([]dns.RR, 0)
	expected.Extra = make([]dns.RR, 0)

	compare_part := func(item1, item2 interface{}, name string) {
		if !reflect.DeepEqual(item1, item2) {
			t.Logf(" * %s is different", name)
			t.Logf("%#v vs %#v", item1, item2)
		}
	}

	// DNS package tends to be loose about some encoding details,
	// only calculating them right before putting the data on the
	// wire.
	sanitize := func(rr_list []dns.RR) {
		for i := range rr_list {
			rr_list[i].Header().Rdlength = 0
		}
	}
	for _, msg := range []*dns.Msg{response, expected} {
		sanitize(msg.Answer)
		sanitize(msg.Ns)
		sanitize(msg.Extra)
	}

	// Confirm equality
	if !reflect.DeepEqual(response, expected) {
		t.Log(response)
		t.Log(expected)
		t.Log("Response not equal to expected response")

		// More DRY to use reflect, but it would also be like
		// chewing broken glass.
		compare_part(response.MsgHdr, expected.MsgHdr, "MsgHdr")
		compare_part(response.Compress, expected.Compress, "Compress")
		compare_part(response.Question, expected.Question, "Question")
		compare_part(response.Answer, expected.Answer, "Answer")
		compare_part(response.Ns, expected.Ns, "Ns")
		compare_part(response.Extra, expected.Extra, "Extra")

		t.FailNow()
	}
}

// Tester for the server internal handling
type RTForHandle struct {
	Server DjdnsServer
}

func (tester RTForHandle) GetResponse(query *dns.Msg) (*dns.Msg, error) {
	return tester.Server.Handle(query)
}
func (tester RTForHandle) WasFailure(resp *dns.Msg, err error) bool {
	return err != nil
}

// Tester for resolving over the network
type RTForNetwork struct {
	Client *dns.Client
	Addr   string
}

func (tester RTForNetwork) GetResponse(query *dns.Msg) (*dns.Msg, error) {
	response, _, err := tester.Client.Exchange(query, tester.Addr)
	return response, err
}
func (tester RTForNetwork) WasFailure(msg *dns.Msg, err error) bool {
	return err != nil || msg.Rcode != dns.RcodeSuccess
}

var resolve_tests = []ResolveTest{
	ResolveTest{
		Description: "Basic request",
		QuestionSection: []dns.Question{
			dns.Question{
				"abcdef.", dns.TypeA, dns.ClassINET},
		},
		ExpectedAnswers: []string{
			"first. A 1.1.1.1",
			"second. A 2.2.2.2",
		},
	},
	ResolveTest{
		Description: "Record not found",
		QuestionSection: []dns.Question{
			dns.Question{
				"def.", dns.TypeA, dns.ClassINET},
		},
		ExpectedAnswers: []string{},
	},
	ResolveTest{
		Description:    "Match the request ID",
		Header:         dns.MsgHdr{Id: 90},
		ExpectedHeader: dns.MsgHdr{Id: 90},
		QuestionSection: []dns.Question{
			dns.Question{
				"def.", dns.TypeA, dns.ClassINET},
		},
		ExpectedAnswers: []string{},
	},
	ResolveTest{
		Description: "Report errors",
		ExpectedHeader: dns.MsgHdr{
			Response: true,
			Rcode:    dns.RcodeServerFailure,
		},
		QuestionSection: []dns.Question{
			dns.Question{
				"evil.", dns.TypeA, dns.ClassINET},
		},
		ShouldFail: true,
	},
	ResolveTest{
		Description: "Recursion",
		QuestionSection: []dns.Question{
			dns.Question{
				"dogbreath.de.", dns.TypeA, dns.ClassINET},
		},
		ExpectedAnswers: []string{
			"only.smells. A 3.3.3.3",
		},
	},
	ResolveTest{
		Description: "Timeout",
		QuestionSection: []dns.Question{
			dns.Question{
				"slow.query", dns.TypeA, dns.ClassINET},
		},
		ShouldFail: true,
	},
}

func Test_DjdnsServer_Handle(t *testing.T) {
	s, _ := setupTestData(nil)
	tester := RTForHandle{s}
	for _, test := range resolve_tests {
		testResolution(t, tester, test)
	}
}

func Test_DjdnsServer_Run(t *testing.T) {
	buf := new(bytes.Buffer)
	s, _ := setupTestData(buf)
	host, port := "127.0.0.1", 9953
	addr := fmt.Sprintf("%s:%d", host, port)

	go func() {
		t.Fatal(s.Run(addr))
	}()
	defer s.Close()
	<-time.After(50 * time.Millisecond)

	c := new(dns.Client)
	tester := RTForNetwork{c, addr}
	for _, test := range resolve_tests {
		testResolution(t, tester, test)
	}

	expected_log := "djdns: Unknown Rtype\n"
	assert.Equal(t, expected_log, buf.String())
}
