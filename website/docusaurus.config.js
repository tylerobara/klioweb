// @ts-check
/** @type {import('@docusaurus/types').Config} */
module.exports = {
  title: 'klioWeb',
  tagline: 'Dashboard for Klio backups of CloudNativePG clusters',
  url: 'https://tylerobara.github.io',
  baseUrl: '/klioweb/',
  organizationName: 'tylerobara',
  projectName: 'klioweb',
  favicon: 'img/favicon.ico',
  onBrokenLinks: 'throw',
  markdown: {hooks: {onBrokenMarkdownLinks: 'throw'}},
  presets: [[
    'classic',
    {
      docs: {routeBasePath: '/', sidebarPath: './sidebars.js', editUrl: 'https://github.com/tylerobara/klioweb/edit/main/website/'},
      blog: false,
      theme: {customCss: './src/css/custom.css'},
    },
  ]],
  themeConfig: {
    navbar: {
      title: 'klioWeb',
      items: [{href: 'https://github.com/tylerobara/klioweb', label: 'GitHub', position: 'right'}],
    },
    footer: {style: 'dark', copyright: 'klioWeb documentation'},
    colorMode: {respectPrefersColorScheme: true},
  },
};
