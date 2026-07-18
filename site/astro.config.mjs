// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// https://astro.build/config
export default defineConfig({
	// TODO: replace with the real domain once purchased and drop the SITE_URL/
	// SITE_BASE env overrides below (used for now to publish under GitHub
	// Pages' project-site path, https://bmoczulski.github.io/yocache/); the
	// site is designed to be served at the domain root (no `base` path).
	site: process.env.SITE_URL ?? 'https://yocache.example.com',
	base: process.env.SITE_BASE ?? '/',
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
				{ label: 'Server configuration', slug: 'server-configuration' },
				{ label: 'Client configuration', slug: 'client-configuration' },
				{ label: 'FAQ', slug: 'faq' },
			],
		}),
	],
});
