import React from 'react';
import ReactDOM from 'react-dom/client';
import { HashRouter, Route, Routes } from 'react-router-dom';
import { App } from './App';
import { Inbox } from './pages/Inbox';
import { Compose } from './pages/Compose';
import './app.css';

// HashRouter rather than BrowserRouter because the renderer is
// loaded from a `file://` URL in production. BrowserRouter's
// HTML5 history API needs a server to serve the same index.html
// for every path, which `file://` doesn't do.
const root = document.getElementById('root');
if (!root) {
  throw new Error('root element missing from index.html');
}

ReactDOM.createRoot(root).render(
  <React.StrictMode>
    <HashRouter>
      <Routes>
        <Route path="/" element={<App />}>
          <Route index element={<Inbox />} />
          <Route path="compose" element={<Compose />} />
          <Route path="mailbox/:mailboxId" element={<Inbox />} />
        </Route>
      </Routes>
    </HashRouter>
  </React.StrictMode>,
);
