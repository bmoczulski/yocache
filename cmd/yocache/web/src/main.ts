import { mount } from 'svelte';
import './app.css';
import App from './App.svelte';

// Svelte 5's mount() appends into the target rather than replacing its
// children — wipe the boot spinner (see index.html) before handing #app
// over to the app.
const target = document.getElementById('app')!;
target.replaceChildren();

const app = mount(App, { target });

export default app;
