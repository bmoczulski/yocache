// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// https://astro.build/config
export default defineConfig({
	site: process.env.SITE_URL ?? 'https://yocache.dev',
	integrations: [
		starlight({
			title: 'YoCache',
			description:
				'Smart cache sharing for Yocto builds — a shared, writable sstate and downloads mirror with automatic uploads.',
			logo: {
				light: './src/assets/yocache-banner-light.svg',
				dark: './src/assets/yocache-banner-dark.svg',
				replacesTitle: true,
			},
			favicon: '/favicon.svg',
			head: [
				// Dark-mode favicon: the light one has dark ink, invisible on a dark tab strip.
				{
					tag: 'link',
					attrs: {
						rel: 'icon',
						href: '/favicon-dark.svg',
						type: 'image/svg+xml',
						media: '(prefers-color-scheme: dark)',
					},
				},
				{
					tag: 'link',
					attrs: { rel: 'apple-touch-icon', href: '/apple-touch-icon.svg' },
				},
				// Social share preview image (link unfurls in Slack, Discord, X, etc.).
				// Generated from scripts/og-image.svg — see site/scripts/generate-og-image.mjs.
				{
					tag: 'meta',
					attrs: { property: 'og:image', content: 'https://yocache.dev/og-image.png' },
				},
				{ tag: 'meta', attrs: { property: 'og:image:width', content: '1200' } },
				{ tag: 'meta', attrs: { property: 'og:image:height', content: '630' } },
				{
					tag: 'meta',
					attrs: { property: 'og:image:alt', content: 'YoCache — Smart Yocto Cache Server' },
				},
				{
					tag: 'meta',
					attrs: { name: 'twitter:image', content: 'https://yocache.dev/og-image.png' },
				},
			],
			social: [{ icon: 'github', label: 'GitHub', href: 'https://github.com/bmoczulski/yocache' }],
			customCss: ['./src/styles/custom.css'],
			sidebar: [
				{ label: 'Getting started', slug: 'getting-started' },
				{ label: 'Why YoCache', slug: 'why-yocache' },
				{ label: 'Running with Docker', slug: 'docker' },
				{ label: 'Server configuration', slug: 'server-configuration' },
				{ label: 'Client configuration', slug: 'client-configuration' },
				{ label: 'FAQ', slug: 'faq' },
			],
		}),
	],
});
