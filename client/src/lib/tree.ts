import type { DiscoveredUrl } from "./types";

export interface TreeNode {
  /** Path segment, e.g. "blog". The host root is "/". */
  name: string;
  /** Full path up to this node, e.g. "/blog/2024". */
  path: string;
  /** URLs that end exactly at this node (several if they differ by query). */
  urls: DiscoveredUrl[];
  children: TreeNode[];
  /** Number of URLs at or below this node. */
  total: number;
}

/**
 * Groups URLs into one path tree per host, so the site's structure can be
 * browsed like a directory listing.
 */
export function buildTrees(urls: DiscoveredUrl[]): Map<string, TreeNode> {
  const roots = new Map<string, TreeNode>();
  // Child lookup by segment, so wide directories stay fast to build.
  const index = new WeakMap<TreeNode, Map<string, TreeNode>>();

  for (const u of urls) {
    let node: TreeNode | undefined = roots.get(u.hostname);
    if (!node) {
      node = newNode("/", "/");
      roots.set(u.hostname, node);
    }
    node.total++;
    let path = "";
    for (const seg of u.path.split("/").filter(Boolean)) {
      path += `/${seg}`;
      let kids = index.get(node);
      if (!kids) {
        kids = new Map();
        index.set(node, kids);
      }
      let child: TreeNode | undefined = kids.get(seg);
      if (!child) {
        child = newNode(seg, path);
        kids.set(seg, child);
        node.children.push(child);
      }
      child.total++;
      node = child;
    }
    node.urls.push(u);
  }

  for (const root of roots.values()) sortTree(root);
  return roots;
}

function newNode(name: string, path: string): TreeNode {
  return { name, path, urls: [], children: [], total: 0 };
}

function sortTree(node: TreeNode) {
  // Sections (nodes with children) first, then alphabetical.
  node.children.sort((a, b) => {
    const ad = a.children.length > 0 ? 0 : 1;
    const bd = b.children.length > 0 ? 0 : 1;
    return ad - bd || a.name.localeCompare(b.name);
  });
  node.children.forEach(sortTree);
}
