package smartfeatures

import "testing"

func msgWith(from string, headers map[string]string) Message {
	m := Message{Headers: headers}
	if from != "" {
		m.From = []Address{{Email: from}}
	}
	return m
}

func TestCategorize(t *testing.T) {
	cases := []struct {
		name string
		msg  Message
		want Category
	}{
		{
			name: "plain person-to-person is primary",
			msg:  msgWith("alice@example.com", nil),
			want: CategoryPrimary,
		},
		{
			name: "mailing list wins over unsubscribe",
			msg: msgWith("list@groups.example.com", map[string]string{
				"List-Id":          "Dev list <dev.example.com>",
				"List-Unsubscribe": "<mailto:unsub@groups.example.com>",
			}),
			want: CategoryForums,
		},
		{
			name: "social domain",
			msg:  msgWith("notify@facebookmail.com", nil),
			want: CategorySocial,
		},
		{
			name: "social subdomain suffix",
			msg:  msgWith("noreply@mail.notifications.linkedin.com", nil),
			want: CategorySocial,
		},
		{
			name: "list-unsubscribe is promotions",
			msg: msgWith("deals@shop.example.com", map[string]string{
				"List-Unsubscribe": "<https://shop.example.com/u/abc>",
			}),
			want: CategoryPromotions,
		},
		{
			name: "precedence bulk is promotions",
			msg: msgWith("news@shop.example.com", map[string]string{
				"Precedence": "bulk",
			}),
			want: CategoryPromotions,
		},
		{
			name: "auto-submitted is updates",
			msg: msgWith("billing@saas.example.com", map[string]string{
				"Auto-Submitted": "auto-generated",
			}),
			want: CategoryUpdates,
		},
		{
			name: "no-reply sender is updates",
			msg:  msgWith("no-reply@saas.example.com", nil),
			want: CategoryUpdates,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Categorize(tc.msg); got != tc.want {
				t.Fatalf("Categorize = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCategoryKeyword(t *testing.T) {
	if got := CategoryPromotions.Keyword(); got != "$category_promotions" {
		t.Fatalf("Keyword = %q", got)
	}
	for _, c := range AllCategories {
		if !c.Valid() {
			t.Fatalf("AllCategories entry %q reported invalid", c)
		}
	}
	if Category("bogus").Valid() {
		t.Fatalf("bogus category reported valid")
	}
}
