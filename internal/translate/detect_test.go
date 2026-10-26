package translate

import "testing"

func TestDetectLanguages(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"chinese", "您好，您的十月份发票已经准备好了，请查看附件并确认付款时间表。", "zh"},
		{"english", "Your invoice for October is ready. Please review the attached files and confirm the payment schedule.", "en"},
		{"japanese", "いつもお世話になっております。来週の会議の日程についてご連絡いたします。", "ja"},
		{"korean", "안녕하세요. 다음 주 회의 일정을 알려드립니다. 첨부 파일을 확인해 주세요.", "ko"},
		{"chinese with english words", "请查收 invoice 和 report，谢谢。会议定在 Friday 下午。", "zh"},
		{"empty", "", ""},
		{"digits only", "1234567890", ""},
	}
	for _, c := range cases {
		if got := Detect(c.text); got != c.want {
			t.Errorf("%s: Detect = %q, want %q", c.name, got, c.want)
		}
	}
}
