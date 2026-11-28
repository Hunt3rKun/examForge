import React from "react";
import ReactDOM from "react-dom/client";
import "katex/dist/katex.min.css";
import "./index.css";
import workerSrc from "pdfjs-dist/build/pdf.worker.mjs?url";
import { GlobalWorkerOptions } from "pdfjs-dist";
import { App } from "./app/App";

GlobalWorkerOptions.workerSrc = workerSrc;
ReactDOM.createRoot(document.getElementById("root")!).render(<React.StrictMode><App /></React.StrictMode>);
