import { createRouter, createWebHistory } from 'vue-router'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: () => import('./views/DashboardView.vue') },
    { path: '/task/:id', component: () => import('./views/TaskView.vue') },
    { path: '/system', component: () => import('./views/SystemView.vue') },
    { path: '/config', component: () => import('./views/ConfigView.vue') },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})
