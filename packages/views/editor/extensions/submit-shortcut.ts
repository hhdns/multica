import { Extension } from "@tiptap/core";

/**
 * `onSubmit` must return true when it actually handled the event and false
 * when there's no submit handler wired up. That lets us fall through to the
 * default Enter behaviour — inserting a newline — when appropriate.
 *
 * `submitOnEnterRef` — checked at keypress time rather than captured at mount.
 * The extension is created once when the editor initialises; a plain boolean
 * would be frozen at that moment. Using a ref lets the value change after mount
 * (e.g. the /api/config response arrives after the first render) without
 * requiring an editor remount.
 */
export function createSubmitExtension(
  onSubmit: () => boolean,
  { submitOnEnterRef }: { submitOnEnterRef: { current: boolean } },
) {
  return Extension.create({
    name: "submitShortcut",
    addKeyboardShortcuts() {
      return {
        "Mod-Enter": () => onSubmit(),
        Enter: () => {
          if (!submitOnEnterRef.current) return false;
          const editor = this.editor;
          // IME guard — never submit while composing a multi-key input
          // (Chinese pinyin, Japanese kana, etc). `view.composing` is set
          // by ProseMirror between compositionstart and compositionend.
          if (editor.view.composing) return false;
          // Let Enter insert a newline inside a code block.
          if (editor.isActive("codeBlock")) return false;
          return onSubmit();
        },
      };
    },
  });
}
