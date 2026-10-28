package htmltext

import "testing"

func TestPlain(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"plain text stays", "Salle B\nPrendre l'ordi", "Salle B\nPrendre l'ordi"},
		{"a lone < is text", "a < b", "a < b"},
		{"Google description",
			`Rejoindre : <a href="https://meet.google.com/abc-defg-hij">https://meet.google.com/abc-defg-hij</a><br><br><b>Ordre du jour</b><ul><li>Budget &amp; planning</li><li>Questions</li></ul>`,
			"Rejoindre : https://meet.google.com/abc-defg-hij\n\nOrdre du jour\n- Budget & planning\n- Questions"},
		{"link text keeps its target", `Voir <a href="https://example.com/doc?a=1&amp;b=2">le document</a>.`,
			"Voir le document (https://example.com/doc?a=1&b=2)."},
		{"mail link showing its address", `<a href="mailto:prof@school.fr">prof@school.fr</a>`, "prof@school.fr"},
		{"empty link text", `Lien : <a href="https://x.test/"></a>`, "Lien : https://x.test/"},
		{"anchors are not targets", `<a href="#top">Haut</a>`, "Haut"},
		{"entities and spaces", "Caf&eacute;&nbsp;&nbsp;  du\n  coin", "Café du coin"},
		{"Outlook document", `<html><head><meta charset="utf-8"><style>p { margin: 0 }</style></head><body>
<div><p class="MsoNormal">Bonjour,</p><p class="MsoNormal">&nbsp;</p><p>Ordre du jour</p></div>
<div>________________</div><div><a href="https://teams.microsoft.com/l/meetup-join/19%3ameeting">Rejoindre la réunion</a></div>
</body></html>`, "Bonjour,\n\nOrdre du jour\n________________\nRejoindre la réunion (https://teams.microsoft.com/l/meetup-join/19%3ameeting)"},
		{"preformatted", "<pre>a\n  b</pre>c", "a\n  b\nc"},
		{"table cells", "<table><tr><td>Lun</td><td>9h</td></tr><tr><td>Mar</td><td>10h</td></tr></table>", "Lun 9h\nMar 10h"},
		{"script left out", "<script>alert(1)</script>Texte", "Texte"},
	} {
		if got := Plain(c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
