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
