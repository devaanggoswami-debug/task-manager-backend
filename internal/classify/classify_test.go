package classify

import (
	"reflect"
	"testing"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		desc      string
		want      string
		defaulted bool
	}{
		{"Fix login bug", "engineering", false},
		{"Prepare Q3 budget forecast", "finance", false},
		{"Send invoice", "finance", false},
		{"Chase overdue invoices and invoicing errors", "finance", false},
		{"Review P&L for October", "finance", false},
		{"pnl review", "finance", false},
		{"Book a demo with the new lead", "sales", false},
		{"Plan the SEO campaign for launch", "marketing", false},
		{"Advertising spend plan", "marketing", false},
		{"Run new ads", "marketing", false},
		{"SOC2 audit evidence", "compliance", false},
		{"GDPR consent banner for the newsletter", "compliance", false},
		{"LAUNCH WEBINAR", "marketing", false},
		{"api-gateway timeouts", "engineering", false},
		{"db cleanup", "engineering", false},
		{"Migrate the database", "engineering", false},

		{"", "engineering", true},
		{"Organise team offsite", "engineering", true},
		{"Update the address book", "engineering", true},
		{"Install dbx tooling", "engineering", true},
		{"Read about apis", "engineering", true},
		{"Taxi booking", "engineering", true},
		{"cid lookup", "engineering", true},
		{"p & l", "engineering", true},

		{"audit budget", "compliance", false},
		{"budget deal", "finance", false},
		{"deal campaign", "sales", false},
		{"campaign bug", "marketing", false},
		{"ads testing", "marketing", false},
		{"fix bug in campaign", "engineering", false},
		{"policy privacy budget forecast revenue", "finance", false},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got, defaulted := Classify(tc.desc)
			if got != tc.want || defaulted != tc.defaulted {
				t.Fatalf("Classify(%q) = %q, %v; want %q, %v", tc.desc, got, defaulted, tc.want, tc.defaulted)
			}
		})
	}
}

func TestWords(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"Fix the API-gateway, v2!", []string{"fix", "the", "api", "gateway", "v2"}},
		{"P&L & tax", []string{"p&l", "tax"}},
		{"Ünïcode wörds", []string{"ünïcode", "wörds"}},
		{"  ", nil},
	}
	for _, tc := range tests {
		if got := Words(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("Words(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
