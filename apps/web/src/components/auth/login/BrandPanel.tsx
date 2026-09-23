import { Bot, ListTodo, Users } from "lucide-react";
import { useTranslation } from "react-i18next";
import {
	DEFAULT_LOGO_HREF,
	resolveBrandLogo,
	resolveBrandName,
} from "@/components/app-shell/branding-effects";
import { useBranding } from "@/hooks/use-branding";

const FEATURES = [
	{
		icon: Bot,
		titleKey: "brand.features.assistant.title",
		descKey: "brand.features.assistant.desc",
	},
	{
		icon: ListTodo,
		titleKey: "brand.features.tasks.title",
		descKey: "brand.features.tasks.desc",
	},
	{
		icon: Users,
		titleKey: "brand.features.collaboration.title",
		descKey: "brand.features.collaboration.desc",
	},
] as const;

export function BrandPanel() {
	const { t } = useTranslation("auth");
	const branding = useBranding();
	const logoUrl = resolveBrandLogo(branding);
	const brandName = resolveBrandName(branding?.brand_name);

	return (
		<div className="relative hidden flex-col justify-center overflow-hidden rounded-l-xl bg-[#0a0a0a] p-10 lg:flex">
			{/* Ambient glow — top, tinted with the brand color */}
			<div className="pointer-events-none absolute -left-24 -top-24 h-72 w-72 rounded-full bg-[radial-gradient(circle,color-mix(in_oklab,var(--palm)_8%,transparent),transparent_60%)]" />

			{/* Decorative concentric rings — right side */}
			<div className="pointer-events-none absolute right-0 top-1/2 h-105 w-105 -translate-y-1/2 translate-x-[42%] rounded-full border border-white/5" />
			<div className="pointer-events-none absolute right-0 top-1/2 h-70 w-70 -translate-y-1/2 translate-x-[42%] rounded-full border border-white/7" />

			<div className="relative">
				{/* Logo + brand */}
				<div className="mb-8 flex items-center gap-3">
					{logoUrl && (
						<img
							src={logoUrl}
							alt={brandName}
							className={
								logoUrl === DEFAULT_LOGO_HREF
									? "h-auto w-40 object-contain"
									: "max-h-9 max-w-24 object-contain"
							}
						/>
					)}
					{logoUrl !== DEFAULT_LOGO_HREF && (
						<span className="text-xl font-bold tracking-tight text-white">
							{brandName}
						</span>
					)}
				</div>

				<h2 className="display-title mb-3 text-3xl font-bold leading-tight text-balance text-white">
					{t("brand.headingPrefix", { brandName })}{" "}
					<span className="text-(--palm)">{t("brand.headingHighlight")}</span>
				</h2>
				<p className="mb-8 text-sm leading-relaxed text-white/55">
					{t("brand.tagline")}
				</p>

				{/* Feature cards */}
				<ul className="space-y-2.5">
					{FEATURES.map(({ icon: Icon, titleKey, descKey }) => (
						<li
							key={titleKey}
							className="flex items-start gap-3.5 rounded-xl border border-white/8 bg-white/4 px-4 py-3.5 transition-colors hover:border-white/[0.14] hover:bg-white/[0.07]"
						>
							<div className="mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-lg bg-[color-mix(in_oklab,var(--palm)_12%,transparent)] ring-1 ring-[color-mix(in_oklab,var(--palm)_20%,transparent)]">
								<Icon className="size-3.5 text-(--palm)" />
							</div>
							<div>
								<p className="text-sm font-semibold text-white/90">
									{t(titleKey)}
								</p>
								<p className="mt-0.5 text-xs leading-relaxed text-white/50">
									{t(descKey)}
								</p>
							</div>
						</li>
					))}
				</ul>
			</div>
		</div>
	);
}
