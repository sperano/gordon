package notice

import "testing"

func TestPRFailedText(t *testing.T) {
	cases := map[string]struct {
		in   PRFailedInfo
		want string
	}{
		"follow-up": {PRFailedInfo{Task: "pr-followup-gordon-7-c5", Message: "pod  `oom`\nkilled", Trigger: "/gordon",
			RunURL: "https://lucarne.example/runs/c/kelos-agents/uid-1"},
			"KELOS TASK `pr-followup-gordon-7-c5` FAILED.\n\nTHE LAST WORD: `pod 'oom' killed`\n\n" +
				"THAT'S NO BACKEND HICCUP. READ THE LOGS BEFORE YOU RETRY. TO RETRY: COMMENT AGAIN WITH A REQUEST " +
				"STARTING WITH `/gordon`: A NEW COMMENT STARTS A NEW FOLLOW-UP. IF THIS FOLLOW-UP DRAFTED THE PULL " +
				"REQUEST, IT STAYS A DRAFT UNTIL THE NEXT ONE MARKS IT READY, OR YOU DO.\n\n" +
				"THE RUN IS ON [LUCARNE](https://lucarne.example/runs/c/kelos-agents/uid-1). SIGN IN AND YOU LAND RIGHT ON IT.\n\n" +
				"WE'VE ALL HAD OUR SOCKS TOSSED AROUND.\n\n<!-- kelos:failed -->"},
		"conflict without a message": {PRFailedInfo{Task: "pr-conflict-0123abcd-7-89abcdef-01234567", Conflict: true,
			Namespace: "kelos-agents"},
			"KELOS TASK `pr-conflict-0123abcd-7-89abcdef-01234567` FAILED.\n\n" +
				"THAT'S NO BACKEND HICCUP. READ THE LOGS BEFORE YOU RETRY. TO RETRY: DELETE THE FAILED TASK " +
				"(`kubectl -n kelos-agents delete task pr-conflict-0123abcd-7-89abcdef-01234567`, OR WAIT FOR ITS TTL); " +
				"THE NEXT CONFLICT SCAN STARTS A NEW ONE WHILE THE CONFLICT LASTS.\n\n" +
				"WE'VE ALL HAD OUR SOCKS TOSSED AROUND.\n\n<!-- kelos:failed -->"},
	}
	for name, c := range cases {
		if got := PRFailedText(c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
	}
}
