import { useEffect } from "react";
import { EditorContent, useEditor, type Editor } from "@tiptap/react";
import StarterKit from "@tiptap/starter-kit";
import Underline from "@tiptap/extension-underline";
import Link from "@tiptap/extension-link";
import Image from "@tiptap/extension-image";
import TextStyle from "@tiptap/extension-text-style";
import Color from "@tiptap/extension-color";
import Highlight from "@tiptap/extension-highlight";

/**
 * Reusable WYSIWYG editor built on TipTap.
 *
 * Owns only the rich-text concern: it renders a formatting toolbar
 * and the editable surface, emits HTML through `onChange`, and (when
 * given an `onImageUpload`) supports inserting and pasting images.
 * Plain-text mode, signature/template wiring, and send-time HTML→cid
 * rewriting all live in the callers so this component stays generic.
 */
export interface RichTextEditorProps {
  value: string;
  onChange: (html: string) => void;
  placeholder?: string;
  /**
   * Upload handler invoked for inserted/pasted images. Returns the
   * `src` to put on the `<img>` (an object URL in Compose, a data
   * URL for signatures). When omitted, image controls are hidden and
   * pasted images are dropped.
   */
  onImageUpload?: (file: File) => Promise<string>;
  ariaLabel?: string;
  minHeight?: number;
}

function ToolbarButton({
  onClick,
  active,
  label,
  title,
}: {
  onClick: () => void;
  active?: boolean;
  label: string;
  title: string;
}) {
  return (
    <button
      type="button"
      // Prevent the editor from losing focus/selection on click.
      onMouseDown={(e) => e.preventDefault()}
      onClick={onClick}
      title={title}
      aria-label={title}
      aria-pressed={active}
      style={{ ...toolbarStyles.button, ...(active ? toolbarStyles.buttonActive : {}) }}
    >
      {label}
    </button>
  );
}

function Toolbar({
  editor,
  onImageUpload,
}: {
  editor: Editor;
  onImageUpload?: (file: File) => Promise<string>;
}) {
  const addLink = () => {
    const prev = editor.getAttributes("link").href as string | undefined;
    const url = window.prompt("Link URL", prev ?? "https://");
    if (url === null) return;
    if (url.trim() === "") {
      editor.chain().focus().extendMarkRange("link").unsetLink().run();
      return;
    }
    editor
      .chain()
      .focus()
      .extendMarkRange("link")
      .setLink({ href: url.trim() })
      .run();
  };

  const pickImage = async (file: File | undefined) => {
    if (!file || !onImageUpload) return;
    try {
      const src = await onImageUpload(file);
      editor.chain().focus().setImage({ src }).run();
    } catch {
      // Surfacing upload errors is the caller's job; keep the editor
      // responsive rather than throwing inside an event handler.
    }
  };

  return (
    <div style={toolbarStyles.bar} role="toolbar" aria-label="Formatting">
      <ToolbarButton
        title="Bold"
        label="B"
        active={editor.isActive("bold")}
        onClick={() => editor.chain().focus().toggleBold().run()}
      />
      <ToolbarButton
        title="Italic"
        label="I"
        active={editor.isActive("italic")}
        onClick={() => editor.chain().focus().toggleItalic().run()}
      />
      <ToolbarButton
        title="Underline"
        label="U"
        active={editor.isActive("underline")}
        onClick={() => editor.chain().focus().toggleUnderline().run()}
      />
      <ToolbarButton
        title="Strikethrough"
        label="S"
        active={editor.isActive("strike")}
        onClick={() => editor.chain().focus().toggleStrike().run()}
      />
      <span style={toolbarStyles.divider} />
      <ToolbarButton
        title="Heading"
        label="H2"
        active={editor.isActive("heading", { level: 2 })}
        onClick={() =>
          editor.chain().focus().toggleHeading({ level: 2 }).run()
        }
      />
      <ToolbarButton
        title="Bulleted list"
        label="• List"
        active={editor.isActive("bulletList")}
        onClick={() => editor.chain().focus().toggleBulletList().run()}
      />
      <ToolbarButton
        title="Numbered list"
        label="1. List"
        active={editor.isActive("orderedList")}
        onClick={() => editor.chain().focus().toggleOrderedList().run()}
      />
      <ToolbarButton
        title="Quote"
        label="❝"
        active={editor.isActive("blockquote")}
        onClick={() => editor.chain().focus().toggleBlockquote().run()}
      />
      <ToolbarButton
        title="Code block"
        label="</>"
        active={editor.isActive("codeBlock")}
        onClick={() => editor.chain().focus().toggleCodeBlock().run()}
      />
      <span style={toolbarStyles.divider} />
      <ToolbarButton title="Link" label="🔗" onClick={addLink} />
      <label
        style={toolbarStyles.colorLabel}
        title="Text colour"
        onMouseDown={(e) => e.preventDefault()}
      >
        A
        <input
          type="color"
          aria-label="Text colour"
          onChange={(e) =>
            editor.chain().focus().setColor(e.target.value).run()
          }
          style={toolbarStyles.colorInput}
        />
      </label>
      <ToolbarButton
        title="Highlight"
        label="🖌"
        active={editor.isActive("highlight")}
        onClick={() => editor.chain().focus().toggleHighlight().run()}
      />
      {onImageUpload && (
        <label
          style={toolbarStyles.button}
          title="Insert image"
          onMouseDown={(e) => e.preventDefault()}
        >
          🖼
          <input
            type="file"
            accept="image/*"
            style={{ display: "none" }}
            onChange={(e) => {
              void pickImage(e.target.files?.[0]);
              e.target.value = "";
            }}
          />
        </label>
      )}
    </div>
  );
}

export default function RichTextEditor({
  value,
  onChange,
  placeholder,
  onImageUpload,
  ariaLabel,
  minHeight = 200,
}: RichTextEditorProps) {
  const editor = useEditor({
    extensions: [
      StarterKit,
      Underline,
      Link.configure({ openOnClick: false, autolink: true }),
      Image.configure({ inline: false, allowBase64: true }),
      TextStyle,
      Color,
      Highlight,
    ],
    content: value || "",
    editorProps: {
      attributes: {
        "aria-label": ariaLabel ?? "Rich text editor",
        style: `min-height:${minHeight}px;outline:none;`,
        role: "textbox",
      },
      handlePaste: (_view, event) => {
        if (!onImageUpload) return false;
        const items = event.clipboardData?.items;
        if (!items) return false;
        for (const item of items) {
          if (item.type.startsWith("image/")) {
            const file = item.getAsFile();
            if (file) {
              event.preventDefault();
              void onImageUpload(file)
                .then((src) => {
                  editor?.chain().focus().setImage({ src }).run();
                })
                .catch(() => {
                  // Mirror the toolbar `pickImage` path: upload errors
                  // are surfaced by the caller; swallow here so a failed
                  // paste doesn't become an unhandled rejection.
                });
              return true;
            }
          }
        }
        return false;
      },
    },
    onUpdate: ({ editor: ed }) => {
      onChange(ed.getHTML());
    },
  });

  // Keep the editor in sync when the caller replaces `value`
  // wholesale (e.g. inserting a template or switching back from
  // plain-text mode). Guard against echoing our own onUpdate.
  useEffect(() => {
    if (!editor) return;
    if (value !== editor.getHTML()) {
      editor.commands.setContent(value || "", false);
    }
  }, [value, editor]);

  return (
    <div style={editorStyles.frame}>
      {editor && <Toolbar editor={editor} onImageUpload={onImageUpload} />}
      <div style={editorStyles.surface}>
        <EditorContent editor={editor} />
        {editor && editor.isEmpty && placeholder && (
          <div style={editorStyles.placeholder} aria-hidden="true">
            {placeholder}
          </div>
        )}
      </div>
    </div>
  );
}

const toolbarStyles: Record<string, React.CSSProperties> = {
  bar: {
    display: "flex",
    flexWrap: "wrap",
    alignItems: "center",
    gap: "0.15rem",
    padding: "0.35rem",
    borderBottom: "1px solid #e5e7eb",
    background: "#f9fafb",
  },
  button: {
    minWidth: "1.9rem",
    padding: "0.2rem 0.4rem",
    fontSize: "0.8rem",
    fontWeight: 600,
    background: "#fff",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    cursor: "pointer",
    color: "#374151",
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
  },
  buttonActive: {
    background: "#dbeafe",
    borderColor: "#2563eb",
    color: "#1d4ed8",
  },
  divider: {
    width: "1px",
    alignSelf: "stretch",
    background: "#e5e7eb",
    margin: "0 0.2rem",
  },
  colorLabel: {
    position: "relative",
    minWidth: "1.9rem",
    padding: "0.2rem 0.4rem",
    fontSize: "0.8rem",
    fontWeight: 700,
    background: "#fff",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    cursor: "pointer",
    color: "#374151",
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
  },
  colorInput: {
    position: "absolute",
    inset: 0,
    opacity: 0,
    cursor: "pointer",
    width: "100%",
    height: "100%",
  },
};

const editorStyles: Record<string, React.CSSProperties> = {
  frame: {
    border: "1px solid #d1d5db",
    borderRadius: "0.375rem",
    overflow: "hidden",
    background: "#fff",
  },
  surface: {
    position: "relative",
    padding: "0.6rem 0.75rem",
  },
  placeholder: {
    position: "absolute",
    top: "0.6rem",
    left: "0.75rem",
    color: "#9ca3af",
    pointerEvents: "none",
    fontSize: "0.9rem",
  },
};
