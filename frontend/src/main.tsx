import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { RouterProvider, createRouter } from "@tanstack/react-router"
import { routeTree } from "./routeTree.gen"
import "./styles/globals.css"

const router = createRouter({ routeTree })

const container = document.getElementById("root")
if (container === null) {
  throw new Error("index.html is missing its #root element")
}

createRoot(container).render(
  <StrictMode>
    <RouterProvider router={router} />
  </StrictMode>,
)
