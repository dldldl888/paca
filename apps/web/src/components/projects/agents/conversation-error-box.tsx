import { AlertTriangle } from "lucide-react";
import { useTranslation } from "react-i18next";

/**
 * A conversation's error, rendered as a card inside the message flow itself
 * — not a page footer, which reads as small print easy to miss entirely
 * below the composer. Backend error_message may contain provider diagnostics
 * or internal context, so show localized recovery guidance instead of raw text.
 *
 * Meant for `Thread`'s `viewportOverlay` slot, which renders inside the
 * scrollable message column right after the last message — so this reads
 * as part of the conversation, not chrome around it.
 */
export function ConversationErrorBox() {
	const { t } = useTranslation("projects");
	return (
		<div className="mb-4 flex items-start gap-2.5 rounded-xl border border-destructive/20 bg-destructive/5 px-3.5 py-3">
			<AlertTriangle className="size-4 shrink-0 text-destructive mt-0.5" />
			<p className="text-sm text-destructive wrap-break-word">
				{t("agents.conversationView.safeError")}
			</p>
		</div>
	);
}
