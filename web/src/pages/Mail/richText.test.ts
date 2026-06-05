/**
 * Unit tests for the HTML <-> plain-text helpers shared by the
 * rich-text composer, signatures and templates.
 */
import { describe, expect, it } from "vitest";

import {
  escapeHtml,
  htmlToPlainText,
  isHtmlEmpty,
  plainTextToHtml,
} from "./richText";

describe("htmlToPlainText", () => {
  it("turns block tags and <br> into newlines and strips markup", () => {
    expect(htmlToPlainText("<p>Hello</p><p>World</p>")).toBe("Hello\nWorld");
    expect(htmlToPlainText("a<br>b")).toBe("a\nb");
  });

  it("renders list items with a leading bullet", () => {
    expect(htmlToPlainText("<ul><li>one</li><li>two</li></ul>")).toBe(
      "- one\n\n- two",
    );
  });

  it("decodes the entities the editor emits", () => {
    expect(htmlToPlainText("<p>a &amp; b &lt;c&gt;</p>")).toBe("a & b <c>");
  });

  it("drops script and style content entirely", () => {
    expect(
      htmlToPlainText("<style>p{}</style><p>visible</p><script>x()</script>"),
    ).toBe("visible");
  });
});

describe("isHtmlEmpty", () => {
  it("treats TipTap's empty paragraph as empty", () => {
    expect(isHtmlEmpty("<p></p>")).toBe(true);
    expect(isHtmlEmpty("   ")).toBe(true);
    expect(isHtmlEmpty("<p>hi</p>")).toBe(false);
  });
});

describe("escapeHtml / plainTextToHtml", () => {
  it("escapes markup-significant characters", () => {
    expect(escapeHtml('<a> & "b"')).toBe("&lt;a&gt; &amp; &quot;b&quot;");
  });

  it("wraps each line in a paragraph and round-trips through text", () => {
    const html = plainTextToHtml("line1\nline2");
    expect(html).toBe("<p>line1</p><p>line2</p>");
    expect(htmlToPlainText(html)).toBe("line1\nline2");
  });

  it("escapes content when converting plain text to HTML", () => {
    expect(plainTextToHtml("a<b>")).toBe("<p>a&lt;b&gt;</p>");
  });
});
